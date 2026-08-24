// Package inject gets text into a live Claude Code session: tmux pane
// discovery and send-keys, a JSONL direct-write fallback for oversized
// post-compact contexts, and a headless `claude --resume --print` last
// resort. Also owns das-blinkenlights (the tmux status-right corner tag
// shown during an inject's lifecycle) and the voice-addressing callsign
// rule, since both are tightly bound to "we just typed into this pane."
//
// Extracted from saturday-mayor so any injector (mayor's own pipeline
// today, a Drive-relay backend later) can reuse the same mechanics without
// duplicating them. Path-selection policy (which of the three paths to try,
// and in what order) stays with the caller — this package exposes the
// mechanics, not the decision.
package inject

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// --- tmux pane discovery ---

// FindTmuxPane locates the tmux pane_id (e.g. "%5") whose pane process tree
// contains a `claude` process running in wantCwd. Returns "" if no tmux
// server is running, no matching pane exists, or wantCwd is empty.
//
// Discovery: `tmux list-panes -aF '#{pane_id} #{pane_pid}'` enumerates
// every pane across every session/window/server. For each pane_pid, BFS
// through descendants via /proc/<pid>/task/<pid>/children and check each
// process's argv[0] for "claude" (the CLI is a Node binary but argv[0] is
// the wrapper script's filename). Once a claude is found, read its
// /proc/<pid>/cwd and match against wantCwd.
//
// Cost: one tmux call + ~tens of /proc reads per inject. Negligible.
func FindTmuxPane(wantCwd string) string {
	if wantCwd == "" {
		return ""
	}
	out, err := exec.Command("tmux", "list-panes", "-aF", "#{pane_id} #{pane_pid}").Output()
	if err != nil {
		return "" // no tmux server, or tmux not installed
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		paneID := parts[0]
		panePid, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		claudePid := findClaudeDescendant(panePid)
		if claudePid == 0 {
			continue
		}
		cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", claudePid))
		if err != nil {
			continue
		}
		if cwd == wantCwd {
			return paneID
		}
	}
	return ""
}

// findClaudeDescendant BFS-walks process descendants of root, returning the
// pid of the first one whose argv contains a "claude" binary. Bounded to
// 200 visited processes so a runaway parent tree can't hang us.
func findClaudeDescendant(root int) int {
	queue := []int{root}
	visited := make(map[int]bool, 32)
	for len(queue) > 0 && len(visited) < 200 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", cur))
		if err == nil {
			args := strings.Split(string(cmdline), "\x00")
			for _, a := range args {
				if a == "claude" || strings.HasSuffix(a, "/claude") {
					return cur
				}
			}
		}
		queue = append(queue, readChildPIDs(cur)...)
	}
	return 0
}

// readChildPIDs returns the immediate child pids of pid, via the procfs
// `children` file (Linux 3.5+).
func readChildPIDs(pid int) []int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return nil
	}
	var out []int
	for _, s := range strings.Fields(string(data)) {
		if n, err := strconv.Atoi(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ViaTmux types text + Enter into a tmux pane. Two send-keys calls: first
// `-l text` writes the literal string into the pane's input buffer (no
// escape interpretation), then a separate `Enter` keystroke submits it. The
// live claude in that pane handles it as if the user typed it. UserPromptSubmit
// hook fires natively, all permissions inherit, scrollback shows everything.
func ViaTmux(paneID, text string) error {
	if err := exec.Command("tmux", "send-keys", "-t", paneID, "-l", text).Run(); err != nil {
		return fmt.Errorf("send-keys text: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", paneID, "Enter").Run(); err != nil {
		return fmt.Errorf("send-keys Enter: %w", err)
	}
	return nil
}

// --- direct-write fallback ---

// TokensSinceLastCompact estimates how many tokens are loaded into the
// model's context window when claude --resume is invoked: bytes from the
// most recent isCompactSummary turn (or beginning of file) to EOF, divided
// by 4. JSONL grows monotonically past compacts, but CC's resume only loads
// the post-compact slice. Used to predict autocompact pressure: when the
// slice is too big, --resume --print autocompacts at load time and diverts
// the inject's response (the buddy-turtle effect).
func TokensSinceLastCompact(jsonlPath string) (int, error) {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	fileSize := info.Size()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var byteOffset int64 = 0
	var lastCompactEndOffset int64 = 0
	for sc.Scan() {
		line := sc.Bytes()
		// Cheap pre-filter before JSON parse.
		if bytes.Contains(line, []byte(`"isCompactSummary":true`)) {
			var t struct {
				IsCompactSummary bool `json:"isCompactSummary"`
			}
			if err := json.Unmarshal(line, &t); err == nil && t.IsCompactSummary {
				lastCompactEndOffset = byteOffset + int64(len(line)) + 1 // +1 for the trailing \n
			}
		}
		byteOffset += int64(len(line)) + 1
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	delta := fileSize - lastCompactEndOffset
	if delta < 0 {
		delta = 0
	}
	return int(delta / 4), nil
}

// lastLeafAndVersion returns the leafUuid from the most recent
// "last-prompt" entry (CC's pointer to the conversation-tree leaf, used as
// parentUuid for direct-written turns) along with the most recent CC
// version string seen on any turn. Mirroring CC's own version keeps
// synthesized entries shaped like whatever just touched the file instead of
// pinning a stale literal that would drift after a CC update. Either field
// may be empty on a fresh session; callers handle empties.
func lastLeafAndVersion(jsonlPath string) (leaf, version string, err error) {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.Contains(line, []byte(`"type":"last-prompt"`)) {
			var t struct {
				Type     string `json:"type"`
				LeafUUID string `json:"leafUuid"`
			}
			if err := json.Unmarshal(line, &t); err == nil &&
				t.Type == "last-prompt" && t.LeafUUID != "" {
				leaf = t.LeafUUID
			}
		}
		if bytes.Contains(line, []byte(`"version":"`)) {
			var t struct {
				Version string `json:"version"`
			}
			if err := json.Unmarshal(line, &t); err == nil && t.Version != "" {
				version = t.Version
			}
		}
	}
	return leaf, version, sc.Err()
}

// genUUID4 returns a UUID v4 string. Stdlib only — no google/uuid dep for a
// single call site.
func genUUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failure is essentially impossible on Linux; fall back
		// to a timestamp-based pseudo-uuid that's still unique enough.
		ts := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(ts >> (8 * (i % 8)))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// DirectWriteUserTurn appends a synthetic user turn + updated last-prompt
// pointer directly to the target session's JSONL, bypassing claude --resume
// --print. Used when the post-compact context size would trigger
// autocompact-on-load and divert the headless response. The sync hook
// surfaces the dangling user turn to the live pane on the user's next
// interaction (framed as "no auto reply — request still pending"), and the
// live claude (with proper live context) handles it correctly.
//
// Schema mirrors what CC itself writes. The `version` field is sampled from
// the most recent live turn in the JSONL so direct-writes track whatever CC
// version is currently touching the file. flock-protected against
// concurrent writers.
func DirectWriteUserTurn(jsonlPath, sessionID, cwd, text string) error {
	f, err := os.OpenFile(jsonlPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open jsonl: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	leaf, ccVersion, err := lastLeafAndVersion(jsonlPath)
	if err != nil {
		return fmt.Errorf("find leaf: %w", err)
	}

	newUUID := genUUID4()
	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	userTurn := map[string]any{
		"isSidechain":    false,
		"promptId":       genUUID4(),
		"type":           "user",
		"message":        map[string]any{"role": "user", "content": text},
		"uuid":           newUUID,
		"timestamp":      timestamp,
		"permissionMode": "bypassPermissions",
		"userType":       "external",
		"entrypoint":     "sdk-cli",
		"cwd":            cwd,
		"gitBranch":      "HEAD",
	}
	if ccVersion != "" {
		userTurn["version"] = ccVersion
	}
	userTurn["sessionId"] = sessionID
	if leaf == "" {
		userTurn["parentUuid"] = nil
	} else {
		userTurn["parentUuid"] = leaf
	}

	lastPrompt := map[string]any{
		"type":       "last-prompt",
		"lastPrompt": text,
		"leafUuid":   newUUID,
		"sessionId":  sessionID,
	}

	enc := json.NewEncoder(f)
	if err := enc.Encode(userTurn); err != nil {
		return fmt.Errorf("write user turn: %w", err)
	}
	if err := enc.Encode(lastPrompt); err != nil {
		return fmt.Errorf("write last-prompt: %w", err)
	}
	return nil
}

// --- headless fallback ---

// Headless runs `claude --resume <sessionID> --print <text>` and returns
// the number of bytes in the assistant's reply. Cwd anchors the JSONL
// resolution — without it the resolver looks under the wrong project dir
// and fails with "No conversation found" (State.Cwd is recorded by the
// watcher from the session's own JSONL events). Stdin is redirected from
// /dev/null: `--print` still reads stdin if attached to a tty, and without
// this the headless inject hangs ~3s waiting on user input even though text
// is already on the command line (see INJECTION.md gotchas).
func Headless(sessionID, cwd, text string) (int, error) {
	cmd := exec.Command("claude", "--resume", sessionID, "--print", text)
	if cwd != "" {
		cmd.Dir = cwd
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, fmt.Errorf("open /dev/null: %w", err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("claude --resume --print: %w", err)
	}
	return len(out), nil
}

// --- voice-addressing callsign rule ---

// CallsignRule is appended to expand-mode injects so CC labels enumerated
// items with phonetically-distinct callsigns. Voice-friendly referent
// grounding — the user can then say "fix the bravo one" and the expander
// can resolve via state.last_assistant_text. Pre-rendered constant to keep
// the pre-pended block byte-stable in CC's prompt cache.
const CallsignRule = "\n\n[saturday: when listing more than one item, label each with a phonetically-distinct callsign — alpha bravo cherry delta echo foxtrot golf hotel — and reuse the same callsign for the same item across this session. Skip for single-item or pure-prose answers.]"

// WithCallsignRule appends CallsignRule to text.
func WithCallsignRule(text string) string {
	return text + CallsignRule
}

// --- das blinkenlights ---
//
// Small overlay on the target CC pane during an inject's lifecycle.
// Implemented via the target tmux session's status-right — tmux owns its
// status line outside the pane's scroll region, so the tag persists across
// CC output without leaving ghost trails (which a prior raw-tty approach
// did).
//
// The session's existing status-right is saved on start and restored on
// stop so user customizations survive the inject cycle.

// Blinker holds the tmux session a StartBlink call tagged, and its
// status-right value from before tagging, so Stop can restore it. The zero
// Blinker is a valid no-op (produced when noBlink is set, paneID is empty,
// or paneID doesn't resolve to a live tmux session).
type Blinker struct {
	session string
	saved   string
}

// resolvePaneSession returns the tmux session that owns paneID, plus its
// current status-right value. Returns ("", "") if paneID isn't a tmux pane
// or tmux isn't available.
func resolvePaneSession(paneID string) (string, string) {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", paneID,
		"#{session_name}").Output()
	if err != nil {
		return "", ""
	}
	session := strings.TrimSpace(string(out))
	if session == "" {
		return "", ""
	}
	saved, err := exec.Command("tmux", "show-option", "-vt", session, "status-right").Output()
	if err != nil {
		return session, ""
	}
	return session, strings.TrimRight(string(saved), "\n")
}

// setBlinkStatus sets session's status-right to a tmux-styled banner. Tmux
// interprets `#[fg=…]…#[default]` natively in status formats.
func setBlinkStatus(session, text, color string, noBlink bool) {
	if noBlink || session == "" {
		return
	}
	formatted := fmt.Sprintf("#[fg=%s,bold]%s#[default]", color, text)
	_ = exec.Command("tmux", "set-option", "-t", session, "status-right", formatted).Run()
}

// clearBlinkStatus restores the original status-right (or unsets it if it
// was empty).
func clearBlinkStatus(session, original string, noBlink bool) {
	if noBlink || session == "" {
		return
	}
	if original == "" {
		_ = exec.Command("tmux", "set-option", "-t", session, "-u", "status-right").Run()
		return
	}
	_ = exec.Command("tmux", "set-option", "-t", session, "status-right", original).Run()
}

// ActiveTag is the in-progress banner; DoneTag fires briefly on completion.
// Project name + braille bar is enough signal — no need to spell out
// "injecting".
func ActiveTag(project string) string {
	return fmt.Sprintf("[⠿⠿⠿⠶⠆ %s]", project)
}

func DoneTag(project string) string {
	return fmt.Sprintf("[✓ %s]", project)
}

// StartBlink looks up the tmux session behind paneID, saves its current
// status-right, and sets the active tag. No goroutine — tmux owns the
// rendering, no rewrite ticker needed (was needed for raw-tty writes that
// got pushed into scrollback by CC's scroll region).
func StartBlink(paneID, project string, noBlink bool) Blinker {
	if noBlink || paneID == "" {
		return Blinker{}
	}
	session, saved := resolvePaneSession(paneID)
	if session == "" {
		return Blinker{}
	}
	setBlinkStatus(session, ActiveTag(project), "colour51", noBlink) // bright cyan
	return Blinker{session: session, saved: saved}
}

// Stop optionally flashes a final banner (e.g. DoneTag), then restores the
// session's original status-right after fadeAfter. If finalText is empty,
// restore is immediate (silent drop). No-op on the zero Blinker.
func (b Blinker) Stop(finalText, finalColor string, fadeAfter time.Duration, noBlink bool) {
	if b.session == "" {
		return
	}
	if finalText != "" {
		setBlinkStatus(b.session, finalText, finalColor, noBlink)
		if fadeAfter > 0 {
			go func() {
				time.Sleep(fadeAfter)
				clearBlinkStatus(b.session, b.saved, noBlink)
			}()
			return
		}
	}
	clearBlinkStatus(b.session, b.saved, noBlink)
}
