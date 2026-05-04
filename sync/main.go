// saturday-sync: Claude Code UserPromptSubmit hook.
//
// Detects out-of-band turns in this session's JSONL (e.g. headless
// `claude --resume --print` injects from Saturday) and emits an
// <earlier-turn channel="voice"> context block so the live pane sees
// them on the user's next interaction.
//
// State: ~/.claude/saturday/cursors/<session_id>.json — (timestamp,
// last_live_prompts ring buffer of size maxLivePrompts). Keyed cursor;
// deterministic note text for cache stability. See ../INJECTION.md for
// the full rationale.
//
// Fail-closed: any error → exit 0 with no output. Hooks must not
// disrupt the live pane.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
}

type turn struct {
	Type             string `json:"type"`
	UUID             string `json:"uuid"`
	ParentUUID       string `json:"parentUuid"`
	Timestamp        string `json:"timestamp"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Message          struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type cursor struct {
	Timestamp       string   `json:"timestamp"`
	LastLivePrompts []string `json:"last_live_prompts,omitempty"`
}

// maxLivePrompts caps the ring buffer of recent live submissions tracked
// per session. Each fire appends in.Prompt and drops the oldest. The
// filter excludes any pair whose user text matches any element. Five
// covers the realistic burst case (user fires several prompts back-to-back
// before the next sync hook fire stabilizes), without growing the cursor
// file unboundedly.
const maxLivePrompts = 5

type pair struct {
	Timestamp string
	UUID      string
	UserText  string
	AsstText  string
}

const replyTrunc = 300

func main() {
	// Hard wall-time ceiling. This hook runs synchronously on the user's
	// prompt-submit path; it must never block CC or balloon RAM. 2s is
	// well above healthy operation (sub-100ms on a 30 MB JSONL) but well
	// below any plausible user-perceptible delay. Acts as a backstop for
	// future input pathologies even if the algorithmic guards regress.
	time.AfterFunc(2*time.Second, func() { os.Exit(0) })

	// `claude --resume --print` (used by Saturday's own injector) also fires
	// UserPromptSubmit. Skip silently — we don't want injects to advance the
	// cursor or emit context into their own headless API call.
	if isHeadless() {
		return
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return
	}
	if in.SessionID == "" || in.TranscriptPath == "" {
		return
	}

	pairs, err := loadPairs(in.TranscriptPath)
	if err != nil || len(pairs) == 0 {
		return
	}

	cursorPath := filepath.Join(os.Getenv("HOME"), ".claude", "saturday", "cursors", in.SessionID+".json")
	cur, exists := loadCursor(cursorPath)

	if !exists {
		// First install on this session: prime cursor, no output.
		saveCursor(cursorPath, cursor{
			Timestamp:       pairs[len(pairs)-1].Timestamp,
			LastLivePrompts: appendPrompt(nil, in.Prompt),
		})
		return
	}

	var unseen []pair
	for _, p := range pairs {
		if p.Timestamp <= cur.Timestamp {
			continue
		}
		// Exclude any of the user's recent live submissions, now landed in JSONL.
		// The single-prompt cursor missed bursts: P1 saved → P2 submitted before
		// next fire → P3 fires, both P1 and P2 are unseen but only P2 matches.
		if containsString(cur.LastLivePrompts, p.UserText) {
			continue
		}
		unseen = append(unseen, p)
	}

	if len(unseen) > 0 {
		emit(os.Stdout, unseen)
	}

	saveCursor(cursorPath, cursor{
		Timestamp:       pairs[len(pairs)-1].Timestamp,
		LastLivePrompts: appendPrompt(cur.LastLivePrompts, in.Prompt),
	})
}

// appendPrompt adds prompt to buf, dropping the oldest entry if the buffer
// would exceed maxLivePrompts. Empty prompts are a no-op.
func appendPrompt(buf []string, prompt string) []string {
	if prompt == "" {
		return buf
	}
	buf = append(buf, prompt)
	if len(buf) > maxLivePrompts {
		buf = buf[len(buf)-maxLivePrompts:]
	}
	return buf
}

func containsString(buf []string, s string) bool {
	for _, x := range buf {
		if x == s {
			return true
		}
	}
	return false
}

// emit writes the frozen <earlier-turn> framing.
// Keep this template stable across versions — it lives in the cached
// 1h prompt prefix for every ongoing session.
func emit(w io.Writer, ps []pair) {
	fmt.Fprintln(w, `<earlier-turn channel="voice">`)
	fmt.Fprintln(w, "Saturday relayed these voice exchanges to this session while you were at the prompt:")
	for _, p := range ps {
		fmt.Fprintf(w, "  voice request: %s\n", oneLine(p.UserText))
		if p.AsstText != "" {
			fmt.Fprintf(w, "  auto reply: %s\n", oneLine(truncate(p.AsstText, replyTrunc)))
		} else {
			fmt.Fprintln(w, "  (no auto reply — request still pending; please act if it hasn't been satisfied above)")
		}
	}
	fmt.Fprintln(w, "These exchanges aren't in the terminal scrollback.")
	fmt.Fprintln(w, `</earlier-turn>`)
}

func loadPairs(path string) ([]pair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	var turns []turn
	for {
		var t turn
		if err := dec.Decode(&t); err == io.EOF {
			break
		} else if err != nil {
			continue
		}
		turns = append(turns, t)
	}

	// Build parent → children map. When mayor's direct-write path appends a
	// user turn while the live CC pane is mid-session, that turn is orphan in
	// the live conversation tree (CC tracks the leaf in-memory and doesn't
	// re-read JSONL mid-run). Pairing such a turn with the next temporally
	// adjacent assistant text would mis-attribute a reply that's actually
	// answering some other prompt — and the framing would then mislead the
	// live claude into thinking the voice request was already handled.
	// Walking parent → child links makes the pairing semantic, not temporal.
	children := make(map[string][]int, len(turns))
	for i, t := range turns {
		if t.UUID != "" && t.ParentUUID != "" {
			children[t.ParentUUID] = append(children[t.ParentUUID], i)
		}
	}

	// findReplyText: BFS through descendants of `rootUUID`, return the first
	// assistant text we hit. Returns empty if no descendants or no asstText
	// in subtree (which is the orphan case — direct-written user turn the
	// live pane hasn't actioned yet).
	//
	// `visited` is load-bearing: CC's JSONL has been observed with duplicate
	// (parent → child) edges (sessions resumed/double-appended), and without
	// it the queue grows exponentially in subtree depth on any orphan turn,
	// blowing past 6 GB RSS within minutes.
	findReplyText := func(rootUUID string) string {
		queue := append([]int(nil), children[rootUUID]...)
		visited := make(map[int]bool)
		for len(queue) > 0 {
			i := queue[0]
			queue = queue[1:]
			if visited[i] {
				continue
			}
			visited[i] = true
			if i >= len(turns) {
				continue
			}
			t := turns[i]
			if t.Type == "assistant" {
				if txt := extractText(t.Message.Content); txt != "" {
					return txt
				}
			}
			queue = append(queue, children[t.UUID]...)
		}
		return ""
	}

	var out []pair
	for _, t := range turns {
		if t.Type != "user" || t.Timestamp == "" {
			continue
		}
		// Skip Claude Code's own meta/control turns: caveat blocks, post-/compact
		// summary, slash commands, and their stdout. These are not voice-channel
		// candidates — they're never injects from Saturday.
		if t.IsMeta || t.IsCompactSummary {
			continue
		}
		userText := extractText(t.Message.Content)
		if userText == "" {
			continue
		}
		if strings.HasPrefix(userText, "<command-name>") || strings.HasPrefix(userText, "<local-command-stdout>") {
			continue
		}
		out = append(out, pair{
			Timestamp: t.Timestamp,
			UUID:      t.UUID,
			UserText:  userText,
			AsstText:  findReplyText(t.UUID),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out, nil
}

func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func loadCursor(path string) (cursor, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cursor{}, false
	}
	// Tolerate the legacy single-prompt schema (last_live_prompt: "...") by
	// reading both shapes off the same object. Migrate forward on next save —
	// new writes only emit last_live_prompts.
	var raw struct {
		Timestamp            string   `json:"timestamp"`
		LastLivePrompts      []string `json:"last_live_prompts"`
		LegacyLastLivePrompt string   `json:"last_live_prompt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return cursor{}, false
	}
	c := cursor{
		Timestamp:       raw.Timestamp,
		LastLivePrompts: raw.LastLivePrompts,
	}
	if len(c.LastLivePrompts) == 0 && raw.LegacyLastLivePrompt != "" {
		c.LastLivePrompts = []string{raw.LegacyLastLivePrompt}
	}
	return c, true
}

func saveCursor(path string, c cursor) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
}

// isHeadless reports whether our parent claude process was invoked with
// --print (i.e. headless `claude --resume --print '<text>'`). Linux-only;
// walks up the /proc tree because the immediate parent may be a shell.
func isHeadless() bool {
	pid := os.Getppid()
	for depth := 0; depth < 8 && pid > 1; depth++ {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return false
		}
		args := strings.Split(string(data), "\x00")
		for _, arg := range args {
			if arg == "--print" || arg == "-p" {
				return true
			}
		}
		// Walk up to grandparent.
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return false
		}
		// stat format: "pid (comm) state ppid ..."
		// Split by ')' to skip comm (which can contain spaces).
		idx := strings.LastIndex(string(stat), ")")
		if idx < 0 {
			return false
		}
		fields := strings.Fields(string(stat[idx+1:]))
		if len(fields) < 2 {
			return false
		}
		next, err := parsePID(fields[1])
		if err != nil || next == pid {
			return false
		}
		pid = next
	}
	return false
}

func parsePID(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad pid: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
