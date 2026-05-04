// saturday-watcher — live state of every active Claude Code session.
//
// Polls ~/.claude/projects/<encoded>/<uuid>.jsonl files at sub-second
// cadence, maintains an in-memory "attentional state" snapshot per
// session, and serves it over a Unix socket so downstream consumers
// (router, expander, mediator) can read current state in O(microseconds).
//
// Lift from lucida/watcher.py: project-name decoding, active-window
// filtering, one-jsonl-per-project canonicalization. Differences:
// 200 ms poll vs 30 s, no LLM in the hot path, in-memory only,
// tail-only on startup with last-N event replay, HTTP-over-unix-socket
// consumer interface.
//
// Usage:
//
//	go run main.go                                     # default everything
//	go run main.go --interval 100ms --active 1h        # tighter poll, wider window
//	curl --unix-socket /tmp/saturday-watcher.sock http://x/state | jq
//	curl --unix-socket /tmp/saturday-watcher.sock http://x/state/lucida | jq
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is baked in at build time via `-ldflags "-X main.version=$(git describe …)"`.
// Local `go build` keeps the "dev" placeholder; buildVersion() then walks
// runtime/debug.ReadBuildInfo() to surface either the installed module
// version (`go install …@vX.Y.Z`) or the VCS revision.
var version = "dev"

func buildVersion() string {
	if version != "dev" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return version
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		return rev + "-dirty"
	}
	return rev
}

// --- Event types: same shape as eval/expander_backtest.go for wire compat ---

type Event struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`
	Message   json.RawMessage `json:"message"`
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type ContentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

type ToolUseSnap struct {
	Name         string `json:"name"`
	InputSummary string `json:"input_summary"`
}

// State is the wire format consumers read. Matches eval/expander_backtest.go.
type State struct {
	Project            string       `json:"project"`
	SessionID          string       `json:"session_id,omitempty"`
	Cwd                string       `json:"cwd,omitempty"`
	LastUserTurn       string       `json:"last_user_turn,omitempty"`
	LastAssistantText  string       `json:"last_assistant_text,omitempty"`
	LastToolUse        *ToolUseSnap `json:"last_tool_use,omitempty"`
	LastToolResultTail string       `json:"last_tool_result_tail,omitempty"`
	ModifiedFiles      []string     `json:"modified_files,omitempty"`
	// V0.2.7: arc summary slot — populated by mayor (slow-loop summarizer),
	// not by watcher. Field exists here only to keep watcher/llmcore wire
	// format aligned per the field-add discipline note in llmcore/state.go.
	SessionArc string `json:"session_arc,omitempty"`
}

// SessionEntry is what the watcher stores per active session.
type SessionEntry struct {
	State       State     `json:"state"`
	LastEventAt time.Time `json:"last_event_at"`
	JSONLPath   string    `json:"jsonl_path"`
	EventsSeen  int       `json:"events_seen"`
	lastOffset  int64
}

// --- Helpers (mirror eval/expander_backtest.go) ---

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// encodedHomePrefix maps a projects-dir root like /home/$USER/.claude/projects
// to "-home-$USER-" — the leading prefix Claude Code uses on encoded project
// dir names under that root. Empty if root doesn't end in /.claude/projects.
func encodedHomePrefix(root string) string {
	home := strings.TrimSuffix(root, "/.claude/projects")
	if home == root {
		return ""
	}
	return "-" + strings.ReplaceAll(strings.TrimPrefix(home, "/"), "/", "-") + "-"
}

func decodeProjectName(encodedDir, homePrefix string) string {
	if homePrefix == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return encodedDir
		}
		homePrefix = "-" + strings.ReplaceAll(strings.TrimPrefix(home, "/"), "/", "-") + "-"
	}
	if !strings.HasPrefix(encodedDir, homePrefix) {
		return encodedDir
	}
	trailing := encodedDir[len(homePrefix):]
	for _, anchor := range []string{"Documents-", "code-", "src-"} {
		if strings.HasPrefix(trailing, anchor) {
			return trailing[len(anchor):]
		}
	}
	return trailing
}

func applyMessageToState(st *State, etype string, msg Message) {
	var asString string
	if err := json.Unmarshal(msg.Content, &asString); err == nil {
		if etype == "user" {
			st.LastUserTurn = head(asString, 500)
		}
		return
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return
	}
	switch etype {
	case "user":
		applyUserBlocks(st, blocks)
	case "assistant":
		applyAssistantBlocks(st, blocks)
	}
}

func applyUserBlocks(st *State, blocks []ContentBlock) {
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			st.LastToolResultTail = extractToolResultTail(b.Content)
		case "text":
			st.LastUserTurn = head(b.Text, 500)
		}
	}
}

func applyAssistantBlocks(st *State, blocks []ContentBlock) {
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				st.LastAssistantText = head(b.Text, 500)
			}
		case "tool_use":
			st.LastToolUse = &ToolUseSnap{
				Name:         b.Name,
				InputSummary: head(string(b.Input), 300),
			}
			if b.Name == "Edit" || b.Name == "Write" {
				if fp := extractFilePath(b.Input); fp != "" {
					addModifiedFile(st, fp)
				}
			}
		}
	}
}

func extractToolResultTail(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return tailStr(s, 400)
	}
	var inner []ContentBlock
	if err := json.Unmarshal(raw, &inner); err == nil && len(inner) > 0 {
		return tailStr(inner[0].Text, 400)
	}
	return ""
}

func extractFilePath(raw json.RawMessage) string {
	var inputObj map[string]any
	if err := json.Unmarshal(raw, &inputObj); err != nil {
		return ""
	}
	fp, _ := inputObj["file_path"].(string)
	return fp
}

func addModifiedFile(st *State, fp string) {
	for _, existing := range st.ModifiedFiles {
		if existing == fp {
			return
		}
	}
	st.ModifiedFiles = append(st.ModifiedFiles, fp)
	if len(st.ModifiedFiles) > 10 {
		st.ModifiedFiles = st.ModifiedFiles[len(st.ModifiedFiles)-10:]
	}
}

// --- Watcher ---

type Watcher struct {
	mu            sync.RWMutex
	sessions      map[string]*SessionEntry // key: jsonl path
	projectsDirs  []string
	activeWindow  time.Duration
	initialReplay int
}

func newWatcher(projectsDirs []string, activeWindow time.Duration, initialReplay int) *Watcher {
	return &Watcher{
		sessions:      map[string]*SessionEntry{},
		projectsDirs:  projectsDirs,
		activeWindow:  activeWindow,
		initialReplay: initialReplay,
	}
}

// scan walks projects dir, finds the most recent jsonl per project that
// is within the active window, and processes any new bytes since last
// scan. On the first call (initial=true), each new session also gets
// the last `initialReplay` events replayed to seed state.
func (w *Watcher) scan(initial bool) {
	cutoff := time.Now().Add(-w.activeWindow)
	for _, dir := range w.projectsDirs {
		homePrefix := encodedHomePrefix(dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, projDir := range entries {
			if !projDir.IsDir() {
				continue
			}
			latest := mostRecentJSONL(filepath.Join(dir, projDir.Name()))
			if latest == "" {
				continue
			}
			info, err := os.Stat(latest)
			if err != nil || info.ModTime().Before(cutoff) {
				continue
			}
			w.processFile(latest, decodeProjectName(projDir.Name(), homePrefix), info, initial)
		}
	}
}

func mostRecentJSONL(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = filepath.Join(dir, e.Name())
		}
	}
	return newest
}

func (w *Watcher) processFile(path, project string, info os.FileInfo, initial bool) {
	w.mu.Lock()
	entry, exists := w.sessions[path]
	if !exists {
		entry = &SessionEntry{
			State:     State{Project: project},
			JSONLPath: path,
		}
		if initial {
			// On startup, skip ahead so we replay only the last ~64KB
			// (semantically: a few dozen events). Avoids reprocessing
			// potentially-huge historical jsonls.
			if info.Size() > 65536 {
				entry.lastOffset = info.Size() - 65536
			}
		} else {
			// New session discovered post-startup: tail-only.
			entry.lastOffset = info.Size()
		}
		w.sessions[path] = entry
	}
	w.mu.Unlock()

	if info.Size() <= entry.lastOffset {
		return
	}

	events, newOffset := readEventsFrom(path, entry.lastOffset)
	if len(events) == 0 {
		return
	}

	// Trim initial replay window to avoid swamping state with old events.
	if initial && len(events) > w.initialReplay {
		events = events[len(events)-w.initialReplay:]
	}

	w.mu.Lock()
	for _, e := range events {
		if e.SessionID != "" {
			entry.State.SessionID = e.SessionID
		}
		if e.Cwd != "" {
			entry.State.Cwd = e.Cwd
		}
		if len(e.Message) > 0 {
			var msg Message
			if err := json.Unmarshal(e.Message, &msg); err == nil {
				applyMessageToState(&entry.State, e.Type, msg)
			}
		}
	}
	entry.EventsSeen += len(events)
	entry.LastEventAt = info.ModTime()
	entry.lastOffset = newOffset
	w.mu.Unlock()

	if !initial {
		log.Printf("[%-12s] +%d events (total=%d, %d bytes)",
			entry.State.Project, len(events), entry.EventsSeen, info.Size())
	}
}

func readEventsFrom(path string, offset int64) ([]Event, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, offset
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var events []Event
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	pos, _ := f.Seek(0, 1)
	return events, pos
}

// prune removes sessions whose JSONL is gone or whose last activity
// predates `staleAfter`. Bounds in-memory map growth across continuous
// daily runs — Claude Code creates a new JSONL per session and never
// reuses paths, so without this the map grows monotonically.
func (w *Watcher) prune(staleAfter time.Duration) int {
	cutoff := time.Now().Add(-staleAfter)
	w.mu.Lock()
	defer w.mu.Unlock()
	removed := 0
	for path, s := range w.sessions {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			delete(w.sessions, path)
			removed++
			continue
		}
		ref := s.LastEventAt
		if ref.IsZero() {
			if info, err := os.Stat(path); err == nil {
				ref = info.ModTime()
			}
		}
		if !ref.IsZero() && ref.Before(cutoff) {
			delete(w.sessions, path)
			removed++
		}
	}
	return removed
}

// --- HTTP-on-unix-socket server ---

func (w *Watcher) listSessions(projectFilter string) []*SessionEntry {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]*SessionEntry, 0, len(w.sessions))
	for _, s := range w.sessions {
		if projectFilter != "" && s.State.Project != projectFilter {
			continue
		}
		copy := *s
		out = append(out, &copy)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastEventAt.After(out[j].LastEventAt)
	})
	return out
}

func (w *Watcher) findSession(id string) *SessionEntry {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, s := range w.sessions {
		if s.State.SessionID == id || s.State.Project == id {
			copy := *s
			return &copy
		}
	}
	return nil
}

func (w *Watcher) handleStateAll(rw http.ResponseWriter, r *http.Request) {
	out := w.listSessions(r.URL.Query().Get("project"))
	rw.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(rw).Encode(out); err != nil {
		log.Printf("encode: %v", err)
	}
}

func (w *Watcher) handleState(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/state/")
	if id == "" {
		http.NotFound(rw, r)
		return
	}
	s := w.findSession(id)
	if s == nil {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(rw).Encode(s); err != nil {
		log.Printf("encode: %v", err)
	}
}

func serve(w *Watcher, sockPath string) error {
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clean stale socket: %w", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if err := os.Chmod(sockPath, 0o666); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/state", w.handleStateAll)
	mux.HandleFunc("/state/", w.handleState)
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("listening on %s", sockPath)
	return server.Serve(ln)
}

// --- main ---

func resolveProjectsDir() string {
	u, err := user.Current()
	if err != nil {
		return os.ExpandEnv("$HOME/.claude/projects")
	}
	return filepath.Join(u.HomeDir, ".claude", "projects")
}

func main() {
	sock := flag.String("sock", "/tmp/saturday-watcher.sock", "Unix socket path for state queries")
	interval := flag.Duration("interval", 200*time.Millisecond, "poll interval")
	activeWindow := flag.Duration("active", 30*time.Minute, "ignore jsonls older than this on initial scan")
	initialReplay := flag.Int("replay", 12, "events to replay from each session on startup")
	roots := flag.String("roots", "", "comma-separated Claude Code projects dirs (default $SATURDAY_ROOTS, else ~/.claude/projects)")
	prune := flag.Duration("prune", 7*24*time.Hour, "drop tracked sessions idle longer than this (mtime/LastEventAt)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(buildVersion())
		return
	}

	rootsSrc := *roots
	if rootsSrc == "" {
		rootsSrc = os.Getenv("SATURDAY_ROOTS")
	}
	var projectsDirs []string
	if rootsSrc == "" {
		projectsDirs = []string{resolveProjectsDir()}
	} else {
		for _, r := range strings.Split(rootsSrc, ",") {
			r = strings.TrimSpace(r)
			if r != "" {
				projectsDirs = append(projectsDirs, r)
			}
		}
	}

	log.Printf("saturday-watcher — roots=%v interval=%s active=%s replay=%d",
		projectsDirs, *interval, *activeWindow, *initialReplay)

	w := newWatcher(projectsDirs, *activeWindow, *initialReplay)

	w.scan(true)
	log.Printf("initial scan loaded %d active sessions", len(w.sessions))
	log.Printf("\033[1;32m[ready] saturday-watcher — sock=%s, %d active sessions, polling every %s\033[0m",
		*sock, len(w.sessions), *interval)

	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go func() {
		if err := serve(w, *sock); err != nil && srvCtx.Err() == nil {
			log.Fatalf("serve: %v", err)
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Printf("shutting down, removing %s", *sock)
		_ = os.Remove(*sock)
		srvCancel()
		os.Exit(0)
	}()

	pruneTicker := time.NewTicker(time.Hour)
	defer pruneTicker.Stop()
	go func() {
		for range pruneTicker.C {
			if n := w.prune(*prune); n > 0 {
				log.Printf("pruned %d stale sessions (idle >%s)", n, *prune)
			}
		}
	}()

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for range ticker.C {
		w.scan(false)
	}
}

// silence unused-import warnings in trimmed builds
var _ = io.Copy
