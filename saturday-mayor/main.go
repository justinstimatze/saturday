// saturday-mayor — V0.1 silent-loop orchestrator.
//
// Reads utterances line-by-line from stdin (V0.1 STT placeholder), queries
// the watcher for active session states, asks the router which session the
// utterance refers to, asks the expander to produce an injectable prompt,
// then exec's `claude --resume <sid> --print '<text>'` headless.
//
// V0.2 will swap stdin for an audio sidecar and add a press-to-commit
// confirmation gate IF VAD/saliency surfaces ambiguous transcripts.
//
// Router/expander prompts, the API plumbing, and the content-hash cache
// live in the saturday/llmcore package — shared with eval/router and
// eval/expander_backtest. See llmcore/llm.go for the cache-key contract.
//
// One mayor process per user. Sequential pipeline — one utterance at a
// time. JSONL-write serialization is implicit because injects don't
// overlap. Concurrent stdin lines are processed FIFO.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	llm "saturday/llmcore"
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

type SessionEntry struct {
	State       llm.State `json:"state"`
	LastEventAt time.Time `json:"last_event_at"`
	JSONLPath   string    `json:"jsonl_path"`
	EventsSeen  int       `json:"events_seen"`
}

// --- Watcher socket query ---

func fetchSessions(sockPath string) ([]SessionEntry, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get("http://x/state")
	if err != nil {
		return nil, fmt.Errorf("watcher %s: %w", sockPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("watcher %s: status %d", sockPath, resp.StatusCode)
	}
	var sessions []SessionEntry
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return sessions, nil
}

// --- Pipeline ---

type Mayor struct {
	apiKey             string
	sockPath           string
	cacheDir           string
	dryRun             bool
	collisionWait      time.Duration // window the JSONL must be stable before injecting
	collisionMax       time.Duration // give up waiting after this and inject anyway
	confThreshold      float64       // skip inject if router or expander conf < this (0 disables)
	askConf            float64       // V0.3: classifier conf >= this AND type==ask → answerAsk path. Default 0.7. Higher = stricter ask gate (more inject false-negatives, fewer ask false-positives).
	injectDirectTokens int           // est. tokens since last compact > this → direct-write skip headless

	// V0.2.1 audio sidecar back-writes. audioMu serializes writes to audioConn
	// so concurrent writers (pipeline narration + state events + Phase 3
	// completion reports from the polling goroutine) don't interleave bytes.
	audioMu   sync.Mutex
	audioConn net.Conn

	// Phase 3 — proactive completion reports. After a successful tmux or
	// direct-write inject, the target session lands in pendingInjects keyed
	// by sessionID. A polling goroutine watches each tracked JSONL: when the
	// latest assistant block is text and the file has been stable for
	// stabilityWindow, it pulls the latest assistant text via the watcher,
	// asks Haiku for a ≤15-word past-tense summary, and speaks it. Trivial
	// tasks (small JSONL growth, fast completion) are filtered out so the
	// user isn't pelted with reports for one-line bash commands.
	pendingMu       sync.Mutex
	pendingInjects  map[string]*pendingInject
	stabilityWindow time.Duration
	completionTTL   time.Duration
	minGrowthBytes  int64
	minElapsed      time.Duration

	// V0.2.6 state socket — exposes mayor's cognitive state to the
	// saturday-thinking TUI renderer (and any other observer). Wire format:
	// line-delimited JSON full snapshots, frame 0 on connect, then on every
	// state change OR 1 Hz heartbeat. Per-conn buffered chan + drop-oldest
	// keeps a slow consumer from stalling mayor.
	stateMu     sync.Mutex
	state       string // idle|hearing|routing|expanding|injecting
	target      string // current target project (set during injecting)
	recent      []RecentUtterance
	turns       int
	startedAt   time.Time
	stateSubsMu sync.Mutex
	stateSubs   map[*stateSub]struct{}
	// V0.2.7: rolling dBFS samples from saturday-audio (5 Hz). Cap 32 ≈ 6.4s.
	// Mirrored into MayorState.Rms on every snapshot.
	rmsRing []float64

	// V0.2.6 corner tag (das blinkenlights). Implemented as tmux session
	// status-right manipulation so the tag lives outside the pane's
	// scroll region (avoids ghost-trail artifacts from raw-tty writes).
	noBlink bool

	// V0.2.7 — slow-loop session arc summarizer. arcSummaries[sessionID] is
	// the latest ≤30-word arc string from llm.RunArc, refreshed in a
	// background goroutine every arcInterval. Read on every expand to enrich
	// target.State.SessionArc before passing to RunExpand. Map is small
	// (one entry per active session) so a single mutex is fine.
	arcMu        sync.Mutex
	arcSummaries map[string]string
	arcInterval  time.Duration

	// V0.3 — expansion-feedback ring. Each successful inject appends a
	// recentInjectRec; on each prompt_submit hook event we Jaccard-match
	// against this ring to detect retypes (user re-typed essentially what
	// we just injected → likely inject misfired or arrived too late).
	// Bounded by recentInjectsMaxAge + recentInjectsCap (see feedback.go).
	recentInjectsMu sync.Mutex
	recentInjects   []recentInjectRec
}

// pendingInject tracks one outstanding inject awaiting a completion signal.
// Lifecycle: created in trackInject, removed in fireCompletion (on report)
// or checkOneInject (on TTL expiry / user-interruption).
type pendingInject struct {
	sessionID          string
	project            string
	jsonlPath          string
	injectText         string
	injectTime         time.Time
	sizeAtInject       int64
	lastSize           int64
	lastSizeChangeTime time.Time
	candidateFired     bool
	candidateText      string // filled by checkOneInject when ready, consumed by fireCompletion

	// V0.2.6: narrate policy for Phase 3 spoken summary. "force" =
	// always speak (skip trivial-drop filter), "silent" = never speak,
	// "auto" = current default (speak if filters pass).
	narrate string

	// V0.2.6 corner tag state. Set by startBlink at trackInject time.
	// Implementation lives in tmux session status-right (see startBlink);
	// blinkSavedStatus holds the user's original status-right value so
	// stopBlink can restore it.
	blinkSession     string
	blinkSavedStatus string
}

// audioWrite serializes JSON event writes to the audio sidecar conn under
// audioMu. Multiple goroutines call this — the synchronous pipeline emits
// state events and narration; the Phase 3 completion poller emits speak
// events asynchronously. Without serialization the writes can interleave at
// the byte level under load. Returns nil silently if no sidecar attached.
func (m *Mayor) audioWrite(evt map[string]any) error {
	m.audioMu.Lock()
	defer m.audioMu.Unlock()
	if m.audioConn == nil {
		return nil
	}
	b, _ := json.Marshal(evt)
	b = append(b, '\n')
	_, err := m.audioConn.Write(b)
	return err
}

// emitState fires a one-shot {"type":"state","activity":...} event over the
// audio sock so the sidecar's spinner reflects mayor's current micro-state.
// Empty activity string = back to idle. No-op if no sidecar attached.
//
// V0.2.6: also updates the state-socket snapshot and broadcasts to thinking
// pane subscribers. Activity strings parse as: "" → idle, "injecting → X" →
// state=injecting target=X, anything else → state=<verbatim> target="".
func (m *Mayor) emitState(activity string) {
	_ = m.audioWrite(map[string]any{"type": "state", "activity": activity})

	state, target := parseActivity(activity)
	m.stateMu.Lock()
	m.state = state
	m.target = target
	m.stateMu.Unlock()
	m.publishState()
}

// --- V0.2.6 state-socket types and helpers ---

// MayorState is the wire-format for the state socket. One frame = one of
// these as single-line JSON + "\n". V is bumped if the schema changes.
type MayorState struct {
	V       int               `json:"v"`
	TS      float64           `json:"ts"`
	UptimeS float64           `json:"uptime_s"`
	State   string            `json:"state"`
	Target  string            `json:"target"`
	Tracked []TrackedInject   `json:"tracked"`
	Recent  []RecentUtterance `json:"recent"`
	Turns   int               `json:"turns"`
	// V0.2.7: rolling mic dBFS samples (oldest → newest), 5 Hz from
	// saturday-audio. Cap 32. -90 = silence floor / muted.
	Rms []float64 `json:"rms"`
}

type TrackedInject struct {
	SID   string  `json:"sid"`
	Proj  string  `json:"proj"`
	AgeS  float64 `json:"age_s"`
	Block string  `json:"block"`
}

type RecentUtterance struct {
	TS    float64 `json:"ts"`
	Text  string  `json:"text"`
	Mode  string  `json:"mode"`
	Route string  `json:"route"`
	Conf  float64 `json:"conf"`
}

// stateSub is one connected subscriber. ch is buffered; drop-oldest on
// overflow keeps mayor unblocked when a slow consumer falls behind.
type stateSub struct {
	ch chan MayorState
}

func parseActivity(activity string) (state, target string) {
	if activity == "" {
		return "idle", ""
	}
	if strings.HasPrefix(activity, "injecting → ") {
		return "injecting", strings.TrimPrefix(activity, "injecting → ")
	}
	return activity, ""
}

func (m *Mayor) snapshot() MayorState {
	m.stateMu.Lock()
	state := m.state
	if state == "" {
		state = "idle"
	}
	target := m.target
	turns := m.turns
	recent := make([]RecentUtterance, len(m.recent))
	copy(recent, m.recent)
	rms := make([]float64, len(m.rmsRing))
	copy(rms, m.rmsRing)
	started := m.startedAt
	m.stateMu.Unlock()

	m.pendingMu.Lock()
	tracked := make([]TrackedInject, 0, len(m.pendingInjects))
	now := time.Now()
	for _, p := range m.pendingInjects {
		block := "running"
		if p.candidateFired {
			block = "text"
		}
		tracked = append(tracked, TrackedInject{
			SID:   p.sessionID,
			Proj:  p.project,
			AgeS:  now.Sub(p.injectTime).Seconds(),
			Block: block,
		})
	}
	m.pendingMu.Unlock()

	return MayorState{
		V:       1,
		TS:      float64(now.UnixNano()) / 1e9,
		UptimeS: now.Sub(started).Seconds(),
		State:   state,
		Target:  target,
		Tracked: tracked,
		Recent:  recent,
		Turns:   turns,
		Rms:     rms,
	}
}

// recordRMS appends a dBFS sample to the rolling ring (cap 32) and
// publishes a fresh state snapshot. Called from handleAudioConn at 5 Hz.
func (m *Mayor) recordRMS(db float64) {
	m.stateMu.Lock()
	m.rmsRing = append(m.rmsRing, db)
	if len(m.rmsRing) > 32 {
		m.rmsRing = m.rmsRing[len(m.rmsRing)-32:]
	}
	m.stateMu.Unlock()
	m.publishState()
}

// publishState broadcasts a fresh snapshot to every subscriber. Drop-oldest
// per-conn so a slow reader can't block mayor's pipeline.
func (m *Mayor) publishState() {
	snap := m.snapshot()
	m.stateSubsMu.Lock()
	subs := make([]*stateSub, 0, len(m.stateSubs))
	for s := range m.stateSubs {
		subs = append(subs, s)
	}
	m.stateSubsMu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- snap:
		default:
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- snap:
			default:
			}
		}
	}
}

// recordUtterance appends to the recent ring buffer (cap 10) and bumps the
// lifetime turn counter, then broadcasts. Called once per utterance after
// the route decision is made.
func (m *Mayor) recordUtterance(text, mode, route string, conf float64) {
	m.stateMu.Lock()
	m.turns++
	m.recent = append(m.recent, RecentUtterance{
		TS:    float64(time.Now().UnixNano()) / 1e9,
		Text:  text,
		Mode:  mode,
		Route: route,
		Conf:  conf,
	})
	if len(m.recent) > 10 {
		m.recent = m.recent[len(m.recent)-10:]
	}
	m.stateMu.Unlock()
	m.publishState()
}

// serveHookSock listens for one-line JSON hook events from saturday-hook
// and dispatches them. V0.2.7 hook contract:
//
//	{"event":"prompt_submit", "session_id":"…", "cwd":"…", "prompt":"…"}
//	{"event":"stop",          "session_id":"…", "cwd":"…"}
//
// Currently logs both and, on stop, looks for a pendingInject keyed by the
// session and accelerates Phase 3 by short-circuiting the JSONL stability
// poll (the hook fires when the assistant turn is genuinely complete, so
// there's no need to re-confirm via stability heuristic).
//
// One conn = one event = close. The helper is fire-and-forget; we never
// reply.
func (m *Mayor) serveHookSock(sockPath string) {
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "hook-sock: clean stale %s: %v\n", sockPath, err)
		return
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook-sock: listen %s: %v\n", sockPath, err)
		return
	}
	if err := os.Chmod(sockPath, 0o666); err != nil {
		fmt.Fprintf(os.Stderr, "hook-sock: chmod %s: %v\n", sockPath, err)
	}
	fmt.Fprintf(os.Stderr, "[hook-sock] listening on %s\n", sockPath)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go m.handleHookConn(conn)
	}
}

func (m *Mayor) handleHookConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 8*1024), 1<<20)
	if !scanner.Scan() {
		return
	}
	line := scanner.Bytes()
	var evt map[string]any
	if err := json.Unmarshal(line, &evt); err != nil {
		fmt.Fprintf(os.Stderr, "\033[2m  hook: malformed: %v\033[0m\n", err)
		return
	}
	event, _ := evt["event"].(string)
	sid, _ := evt["session_id"].(string)
	switch event {
	case "prompt_submit":
		prompt, _ := evt["prompt"].(string)
		// Dim log: useful for correlating user-typed vs voice-injected
		// prompts when debugging the inject pipeline. Trimmed to one line.
		fmt.Fprintf(os.Stderr, "\033[2m  hook · prompt_submit · %s · %q\033[0m\n",
			head(sid, 8), oneLine(head(prompt, 80)))
		// V0.3 expansion-feedback: did the user just retype something we
		// recently injected into this same session? If so, inject was
		// likely wrong / late / swallowed. Log + persist for later tuning.
		if rec, sim, isRetype := m.checkRetype(sid, prompt); isRetype {
			age := time.Since(rec.TS)
			fmt.Fprintf(os.Stderr,
				"\033[35m  feedback · retype\033[0m · %s · sim=%.2f · %s ago\n  \033[2minject:\033[0m %q\n  \033[2mtyped:\033[0m  %q\n",
				rec.Project, sim, age.Round(time.Second), oneLine(head(rec.Text, 80)), oneLine(head(prompt, 80)))
			appendFeedbackRec(map[string]any{
				"ts":                 float64(time.Now().UnixNano()) / 1e9,
				"event":              "retype",
				"session_id":         sid,
				"project":            rec.Project,
				"inject_text":        rec.Text,
				"prompt_text":        prompt,
				"similarity":         sim,
				"inject_age_seconds": age.Seconds(),
			})
		}
	case "stop":
		// If we have a pendingInject for this session, fire Phase 3
		// immediately. Stability poll was the JSONL-only proxy for "turn
		// done"; the Stop hook is the authoritative signal.
		m.pendingMu.Lock()
		p, ok := m.pendingInjects[sid]
		m.pendingMu.Unlock()
		if !ok {
			fmt.Fprintf(os.Stderr, "\033[2m  hook · stop · %s · (no tracked inject)\033[0m\n", head(sid, 8))
			return
		}
		fmt.Fprintf(os.Stderr, "\033[2m  hook · stop · %s · %s · firing Phase 3\033[0m\n",
			head(sid, 8), p.project)
		// Trigger an immediate completion check for this inject. checkOneInject
		// already handles the user-message gate + assistant-block-text check;
		// the hook just removes the stability-window wait.
		p.lastSizeChangeTime = time.Now().Add(-m.stabilityWindow - time.Second)
		m.checkOneInject(p)
	default:
		fmt.Fprintf(os.Stderr, "\033[2m  hook · unknown event %q\033[0m\n", event)
	}
}

// head returns the first n chars of s, or s if shorter. Local copy to avoid
// importing watcher's helper across modules.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// serveStateSock listens on the Unix socket and dispatches each incoming
// connection to handleStateConn. Listener errors are logged once; mayor
// keeps running without the state socket if the socket can't be opened.
func (m *Mayor) serveStateSock(sockPath string) {
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "state-sock: clean stale %s: %v\n", sockPath, err)
		return
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "state-sock: listen %s: %v\n", sockPath, err)
		return
	}
	if err := os.Chmod(sockPath, 0o666); err != nil {
		fmt.Fprintf(os.Stderr, "state-sock: chmod %s: %v\n", sockPath, err)
	}
	fmt.Fprintf(os.Stderr, "[state-sock] listening on %s\n", sockPath)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go m.handleStateConn(conn)
	}
}

func (m *Mayor) handleStateConn(conn net.Conn) {
	defer conn.Close()
	sub := &stateSub{ch: make(chan MayorState, 16)}
	m.stateSubsMu.Lock()
	if m.stateSubs == nil {
		m.stateSubs = map[*stateSub]struct{}{}
	}
	m.stateSubs[sub] = struct{}{}
	m.stateSubsMu.Unlock()
	defer func() {
		m.stateSubsMu.Lock()
		delete(m.stateSubs, sub)
		m.stateSubsMu.Unlock()
	}()

	if err := writeStateFrame(conn, m.snapshot()); err != nil {
		return
	}
	hb := time.NewTicker(1 * time.Second)
	defer hb.Stop()
	for {
		select {
		case snap := <-sub.ch:
			if err := writeStateFrame(conn, snap); err != nil {
				return
			}
		case <-hb.C:
			if err := writeStateFrame(conn, m.snapshot()); err != nil {
				return
			}
		}
	}
}

func writeStateFrame(conn net.Conn, snap MayorState) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = conn.Write(b)
	return err
}

// --- V0.2.6 corner tag (das blinkenlights) ---
//
// Small overlay on the target CC pane during inject lifecycle. Implemented
// via the target tmux session's status-right — tmux owns its status line
// outside the pane's scroll region, so the tag persists across CC output
// without leaving ghost trails (which the prior raw-tty approach did).
//
// We save the session's existing status-right on start and restore it on
// stop so user customizations survive the inject cycle.

// resolvePaneSession returns the tmux session that owns paneID, plus the
// current status-right value (so we can restore it on stop). Returns
// ("", "") if paneID isn't a tmux pane or tmux isn't available.
func (m *Mayor) resolvePaneSession(paneID string) (string, string) {
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

// setBlinkStatus sets the target session's status-right to a tmux-styled
// banner. Tmux interprets `#[fg=…]…#[default]` natively in status formats.
func (m *Mayor) setBlinkStatus(session, text, color string) {
	if m.noBlink || session == "" {
		return
	}
	formatted := fmt.Sprintf("#[fg=%s,bold]%s#[default]", color, text)
	_ = exec.Command("tmux", "set-option", "-t", session, "status-right", formatted).Run()
}

// clearBlinkStatus restores the original status-right (or unsets if it
// was empty).
func (m *Mayor) clearBlinkStatus(session, original string) {
	if m.noBlink || session == "" {
		return
	}
	if original == "" {
		_ = exec.Command("tmux", "set-option", "-t", session, "-u", "status-right").Run()
		return
	}
	_ = exec.Command("tmux", "set-option", "-t", session, "status-right", original).Run()
}

// activeTag is the in-progress banner; doneTag fires briefly on completion.
// Project name + braille bar is enough signal — no need to spell out
// "injecting".
func activeTag(project string) string {
	return fmt.Sprintf("[⠿⠿⠿⠶⠆ %s]", project)
}

func doneTag(project string) string {
	return fmt.Sprintf("[✓ %s]", project)
}

// startBlink looks up the target tmux session, saves its current
// status-right, and sets the active tag. No goroutine — tmux owns the
// rendering, no rewrite ticker needed (was needed for raw-tty writes that
// got pushed into scrollback by CC's scroll region).
func (m *Mayor) startBlink(p *pendingInject, paneID string) {
	if m.noBlink || p == nil || paneID == "" {
		return
	}
	session, saved := m.resolvePaneSession(paneID)
	if session == "" {
		return
	}
	m.pendingMu.Lock()
	p.blinkSession = session
	p.blinkSavedStatus = saved
	m.pendingMu.Unlock()
	m.setBlinkStatus(session, activeTag(p.project), "colour51") // bright cyan
}

// stopBlink optionally flashes a final banner (e.g. done), then restores
// the session's original status-right after fadeAfter. If finalText is
// empty, restore is immediate (silent drop).
func (m *Mayor) stopBlink(p *pendingInject, finalText, finalColor string, fadeAfter time.Duration) {
	if p == nil {
		return
	}
	m.pendingMu.Lock()
	session := p.blinkSession
	saved := p.blinkSavedStatus
	p.blinkSession = ""
	m.pendingMu.Unlock()
	if session == "" {
		return
	}
	if finalText != "" {
		m.setBlinkStatus(session, finalText, finalColor)
		if fadeAfter > 0 {
			go func() {
				time.Sleep(fadeAfter)
				m.clearBlinkStatus(session, saved)
			}()
			return
		}
	}
	m.clearBlinkStatus(session, saved)
}

// getPending fetches a pending inject by sessionID under lock. Returns
// nil if not present.
func (m *Mayor) getPending(sessionID string) *pendingInject {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return m.pendingInjects[sessionID]
}

func getInt(m map[string]any, k string) (int, bool) {
	switch v := m[k].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

func getStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func getFloat(m map[string]any, k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}

// stripWakeWord detects a leading "saturday" / "hey saturday" wake word and
// returns the utterance with the prefix removed plus a true flag. The
// follow-on character must be whitespace or end-of-string punctuation so
// that "saturdayfile.go" doesn't accidentally trigger. Bare "saturday"
// (with nothing after) returns "" + true — caller should still route to
// ask-mode and the asker will produce a generic "what's on" reply.
func stripWakeWord(utt string) (string, bool) {
	s := strings.TrimSpace(utt)
	lower := strings.ToLower(s)
	for _, prefix := range []string{"hey saturday", "saturday"} {
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		rest := s[len(prefix):]
		if rest == "" {
			return "", true
		}
		sep := rest[0]
		if sep == ' ' || sep == ',' || sep == ':' || sep == ';' ||
			sep == '!' || sep == '.' || sep == '?' || sep == '-' || sep == '\t' {
			return strings.TrimLeft(rest, " \t,:;!.?-"), true
		}
	}
	return s, false
}

// handle runs one utterance through the pipeline. mode is "expand" or
// "verbatim". Verbatim skips the LLM expander; the utterance text becomes
// the inject directly. Router still picks the session in both modes.
// narrate is "force"|"silent"|"auto" — controls whether Phase 3 fires the
// spoken completion summary (force=always, silent=never, auto=current
// trivial-drop filters apply).
//
// V0.3 ask-mode branch: utterances directed AT Saturday (wake-word prefix
// or classifier-flagged) skip the route/expand/inject pipeline entirely
// and instead get a spoken answer from cross-session arcs + recent state.
// Verbatim mode bypasses the branch — user explicitly typed literal text.
func (m *Mayor) handle(utterance, mode, narrate string) error {
	if mode != "verbatim" {
		if cleaned, isAsk := stripWakeWord(utterance); isAsk {
			fmt.Fprintf(os.Stderr, "\033[35m? ask\033[0m \033[2m(wake-word)\033[0m\n")
			return m.answerAsk(cleaned)
		}
		// Classifier — Haiku call, ~$0.0001 per utterance. Errors fall
		// through silently to inject; classifier is a UX optimization,
		// not a load-bearing decision.
		t, conf, rat, err := llm.RunClassify(m.apiKey, m.cacheDir, utterance)
		if err == nil {
			if t == "ask" && conf >= m.askConf {
				fmt.Fprintf(os.Stderr, "\033[35m? ask\033[0m \033[2m(conf=%.2f — %s)\033[0m\n",
					conf, oneLine(rat))
				return m.answerAsk(utterance)
			}
			if t == "ask" {
				fmt.Fprintf(os.Stderr, "  \033[2m↳ classifier ask conf=%.2f below %.2f; treating as inject\033[0m\n",
					conf, m.askConf)
			}
		}
	}

	m.emitState("routing")
	defer m.emitState("")
	sessions, err := fetchSessions(m.sockPath)
	if err != nil {
		return fmt.Errorf("fetch sessions: %w", err)
	}
	// Filter: must have a session_id (some entries may be project-only stubs)
	live := make([]SessionEntry, 0, len(sessions))
	for _, s := range sessions {
		if s.State.SessionID != "" {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return errors.New("no active sessions in watcher state")
	}
	if len(live) == 1 {
		// Single-session shortcut: skip the router, just expand against the only target.
		m.recordUtterance(utterance, mode, live[0].State.Project, 0)
		return m.expandAndInject(utterance, live[0], mode, narrate)
	}

	cands := make([]llm.State, len(live))
	for i, s := range live {
		cands[i] = s.State
		// V0.2.8: enrich routing candidates with the cached arc summary so
		// the router can disambiguate anaphoric references ("rerun it",
		// "the same one") by session theme, not just last-N-turn signals.
		// No-op if no arc cached yet; expander still re-enriches its target.
		m.enrichWithArc(&cands[i])
	}
	rt, err := llm.RunRoute(m.apiKey, m.cacheDir, utterance, cands)
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	idx, ok := getInt(rt, "target_index")
	if !ok || idx < 0 || idx >= len(live) {
		return fmt.Errorf("router returned bad target_index: %v", rt["target_index"])
	}
	conf := getFloat(rt, "confidence")
	target := live[idx]
	fmt.Fprintf(os.Stderr, "\033[2;36m→ route:\033[0m %s \033[2m(conf=%.2f)\033[0m \033[2m— %s\033[0m\n",
		target.State.Project, conf, oneLine(getStr(rt, "rationale")))
	if m.confThreshold > 0 && conf < m.confThreshold {
		fmt.Fprintf(os.Stderr, "  ↳ router conf below threshold %.2f; skipping inject\n", m.confThreshold)
		return nil
	}
	m.recordUtterance(utterance, mode, target.State.Project, conf)
	return m.expandAndInject(utterance, target, mode, narrate)
}

// answerAsk is the V0.3 ask-mode path: gather Saturday's bird's-eye state
// (arcs, recent voice activity, in-flight injects) and call llm.RunAsk to
// produce a brief spoken answer. The answer goes to the audio sidecar for
// TTS and is logged to mayor's stderr in the ask register (magenta). No
// pendingInject is created — ask is a terminal action.
func (m *Mayor) answerAsk(utterance string) error {
	m.emitState("asking")
	defer m.emitState("")

	if utterance == "" {
		// Bare wake word ("saturday") with no follow-on — treat as a
		// generic "what's on" probe rather than failing.
		utterance = "what's on"
	}

	ctx := llm.AskContext{
		Arcs:         map[string]string{},
		ProjectBySID: map[string]string{},
	}

	// Pair each cached arc with its project name from the watcher
	// snapshot. Sessions with no arc cached yet are omitted — RunAsk's
	// prompt tells the model how to handle absences.
	if sessions, err := fetchSessions(m.sockPath); err == nil {
		m.arcMu.Lock()
		for _, s := range sessions {
			sid := s.State.SessionID
			if sid == "" {
				continue
			}
			if arc, ok := m.arcSummaries[sid]; ok && arc != "" {
				ctx.Arcs[sid] = arc
				ctx.ProjectBySID[sid] = s.State.Project
			}
		}
		m.arcMu.Unlock()
	}

	m.stateMu.Lock()
	for _, u := range m.recent {
		ctx.RecentUtterances = append(ctx.RecentUtterances,
			fmt.Sprintf("%s → %s (%s)", oneLine(u.Text), u.Route, u.Mode))
	}
	m.stateMu.Unlock()

	m.pendingMu.Lock()
	for _, p := range m.pendingInjects {
		ctx.TrackedInjects = append(ctx.TrackedInjects,
			fmt.Sprintf("%s: %s", p.project, oneLine(head(p.injectText, 80))))
	}
	m.pendingMu.Unlock()

	reply, err := llm.RunAsk(m.apiKey, m.cacheDir, utterance, ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\033[31m× ask:\033[0m %v\n", err)
		return err
	}
	fmt.Fprintf(os.Stderr, "\033[35m? ask:\033[0m %q\n  \033[35m→\033[0m %s\n",
		oneLine(utterance), reply)

	// Mark the utterance with mode=ask so the thinking pane and
	// downstream tools can distinguish ask traffic from inject traffic.
	m.recordUtterance(utterance, "ask", "saturday", 1.0)

	if err := m.audioWrite(map[string]any{"type": "speak", "text": reply}); err != nil {
		fmt.Fprintf(os.Stderr, "  \033[2m↳ audio speak failed: %v\033[0m\n", err)
	}
	return nil
}

// commitInject runs the inject-execution path: collision-wait, optional TTS
// narration, path selection (tmux → direct-write → headless). Used by both
// expand-mode (after the expander returns action=inject) and verbatim-mode
// (utterance text becomes the inject directly, narration empty).
func (m *Mayor) commitInject(target SessionEntry, text, narration, narrate string) error {
	m.emitState("injecting → " + target.State.Project)
	if m.dryRun {
		fmt.Fprintln(os.Stderr, "  [dry-run; skipping exec]")
		return nil
	}
	if narration != "" {
		if err := m.audioWrite(map[string]any{"type": "speak", "text": narration}); err == nil {
			fmt.Fprintf(os.Stderr, "  ↳ narrating: %q\n", narration)
		}
	}
	waited, timedOut := m.waitForQuiet(target.JSONLPath)
	if waited > 0 {
		tag := "stable"
		if timedOut {
			tag = "timed-out"
		}
		fmt.Fprintf(os.Stderr, "  ↳ collision-window %s after %s\n", tag, waited.Round(time.Millisecond))
	}
	// Path selection (preferred → fallback):
	// 1. Target's claude is running in a tmux pane → tmux send-keys.
	// 2. No tmux pane, JSONL post-compact size > threshold → direct-write user turn.
	// 3. Else → headless `claude --resume --print`.
	if paneID := findTmuxPane(target.State.Cwd); paneID != "" {
		fmt.Fprintf(os.Stderr, "  ↳ found tmux pane %s for cwd=%s; using tmux send-keys\n", paneID, target.State.Cwd)
		if err := injectViaTmux(paneID, text); err != nil {
			return fmt.Errorf("tmux send-keys: %w", err)
		}
		fmt.Fprintln(os.Stderr, "  ↳ injected via tmux send-keys (live pane handles)")
		m.trackInject(target, text, narrate)
		m.startBlink(m.getPending(target.State.SessionID), paneID)
		return nil
	}
	fmt.Fprintln(os.Stderr, "  ↳ no tmux pane found for target cwd; using JSONL fallback path")
	if m.injectDirectTokens > 0 && target.JSONLPath != "" {
		est, err := tokensSinceLastCompact(target.JSONLPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ↳ token-estimate failed: %v; falling back to headless inject\n", err)
		} else if est > m.injectDirectTokens {
			fmt.Fprintf(os.Stderr, "  ↳ post-compact tokens-est %d > %d threshold; direct-writing user turn (no headless invocation)\n",
				est, m.injectDirectTokens)
			if err := directWriteUserTurn(target.JSONLPath, target.State.SessionID, target.State.Cwd, text); err != nil {
				return fmt.Errorf("direct-write: %w", err)
			}
			fmt.Fprintf(os.Stderr, "  ↳ direct-wrote user turn to %s\n", target.JSONLPath)
			m.trackInject(target, text, narrate)
			return nil
		} else {
			fmt.Fprintf(os.Stderr, "  ↳ post-compact tokens-est %d ≤ %d; using headless inject\n",
				est, m.injectDirectTokens)
		}
	}
	return m.inject(target.State.SessionID, target.State.Cwd, text)
}

// callsignRule is appended to expand-mode injects so CC labels enumerated
// items with phonetically-distinct callsigns. Voice-friendly referent
// grounding — the user can then say "fix the bravo one" and the expander
// can resolve via state.last_assistant_text. Skipped for verbatim mode
// because the user typed the literal text. Pre-rendered constant to keep
// the pre-pended block byte-stable in CC's prompt cache.
const callsignRule = "\n\n[saturday: when listing more than one item, label each with a phonetically-distinct callsign — alpha bravo cherry delta echo foxtrot golf hotel — and reuse the same callsign for the same item across this session. Skip for single-item or pure-prose answers.]"

func withCallsignRule(text string) string {
	return text + callsignRule
}

func (m *Mayor) expandAndInject(utterance string, target SessionEntry, mode, narrate string) error {
	if mode == "verbatim" {
		// Verbatim mode: utterance text becomes the inject directly. No
		// expander LLM call. No narration speak event — sidecar's instant
		// stock ack is the audible feedback, no need for a second TTS.
		fmt.Fprintf(os.Stderr, "\033[1;33m→ Saturday → %s\033[0m \033[2m(verbatim)\033[0m: \033[33m%s\033[0m\n",
			target.State.Project, oneLine(utterance))
		return m.commitInject(target, utterance, "", narrate)
	}
	m.emitState("expanding")
	m.enrichWithArc(&target.State)
	exp, err := llm.RunExpand(m.apiKey, m.cacheDir, utterance, target.State)
	if err != nil {
		return fmt.Errorf("expander: %w", err)
	}
	action := getStr(exp, "action")
	text := getStr(exp, "text")
	conf := getFloat(exp, "confidence")
	switch action {
	case "inject":
		fmt.Fprintf(os.Stderr, "\033[1;33m→ Saturday → %s\033[0m \033[2m(conf=%.2f)\033[0m: \033[33m%s\033[0m\n",
			target.State.Project, conf, oneLine(text))
		if m.confThreshold > 0 && conf < m.confThreshold {
			fmt.Fprintf(os.Stderr, "  ↳ expander conf below threshold %.2f; skipping inject\n", m.confThreshold)
			return nil
		}
		text = withCallsignRule(text)
		return m.commitInject(target, text, getStr(exp, "confirmation"), narrate)
	case "ask":
		fmt.Fprintf(os.Stderr, "\033[1;35m? expander asks\033[0m \033[2m(%s)\033[0m: %s\n", target.State.Project, oneLine(text))
		if err := m.audioWrite(map[string]any{"type": "speak", "text": text}); err != nil {
			fmt.Fprintf(os.Stderr, "  ↳ tts send failed: %v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "  ↳ spoken via TTS")
		}
		return nil
	case "decline":
		fmt.Fprintf(os.Stderr, "\033[1;31m✗ expander declined\033[0m \033[2m(%s)\033[0m: \033[2m%s\033[0m\n", target.State.Project, oneLine(getStr(exp, "rationale")))
		return nil
	default:
		return fmt.Errorf("expander returned unknown action: %q", action)
	}
}

// findTmuxPane locates the tmux pane_id (e.g. "%5") whose pane process
// tree contains a `claude` process running in wantCwd. Returns "" if no
// tmux server is running, no matching pane exists, or wantCwd is empty.
//
// Discovery: `tmux list-panes -aF '#{pane_id} #{pane_pid}'` enumerates
// every pane across every session/window/server. For each pane_pid, BFS
// through descendants via /proc/<pid>/task/<pid>/children and check each
// process's argv[0] for "claude" (the CLI is a Node binary but argv[0]
// is the wrapper script's filename). Once a claude is found, read its
// /proc/<pid>/cwd and match against wantCwd.
//
// Cost: one tmux call + ~tens of /proc reads per inject. Negligible.
func findTmuxPane(wantCwd string) string {
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

// findClaudeDescendant BFS-walks process descendants of root, returning
// the pid of the first one whose argv contains a "claude" binary. Bounded
// to 200 visited processes so a runaway parent tree can't hang us.
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

// readChildPIDs returns the immediate child pids of pid, via the
// procfs `children` file (Linux 3.5+).
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

// injectViaTmux types the expanded text + Enter into a tmux pane.
// Two send-keys calls: first `-l <text>` writes the literal string into
// the pane's input buffer (no escape interpretation), then a separate
// `Enter` keystroke submits it. The live claude in that pane handles it
// as if the user typed it. UserPromptSubmit hook fires natively, all
// permissions inherit, scrollback shows everything.
func injectViaTmux(paneID, text string) error {
	if err := exec.Command("tmux", "send-keys", "-t", paneID, "-l", text).Run(); err != nil {
		return fmt.Errorf("send-keys text: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", paneID, "Enter").Run(); err != nil {
		return fmt.Errorf("send-keys Enter: %w", err)
	}
	return nil
}

// tokensSinceLastCompact estimates how many tokens are loaded into the
// model's context window when claude --resume is invoked: bytes from the
// most recent isCompactSummary turn (or beginning of file) to EOF, divided
// by 4. JSONL grows monotonically past compacts, but CC's resume only
// loads the post-compact slice. Used to predict autocompact pressure: when
// the slice is too big, --resume --print autocompacts at load time and
// diverts the inject's response (the buddy-turtle effect).
func tokensSinceLastCompact(jsonlPath string) (int, error) {
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
// "last-prompt" entry (CC's pointer to the conversation-tree leaf, used
// as parentUuid for direct-written turns) along with the most recent
// CC version string seen on any turn. Mirroring CC's own version keeps
// our synthesized entries shaped like whatever just touched the file
// instead of pinning a stale literal that would drift after a CC update.
// Either field may be empty on a fresh session; callers handle empties.
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

// genUUID4 returns a UUID v4 string. Stdlib only — no google/uuid dep
// for a single call site.
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

// directWriteUserTurn appends a synthetic user turn + updated last-prompt
// pointer directly to the target session's JSONL, bypassing claude
// --resume --print. Used when the post-compact context size would trigger
// autocompact-on-load and divert the headless response. The sync hook
// surfaces the dangling user turn to the live pane on the user's next
// interaction (framed as "no auto reply — request still pending"), and
// the live claude (with proper live context) handles it correctly.
//
// Schema mirrors what CC itself writes. The `version` field is sampled
// from the most recent live turn in the JSONL so direct-writes track
// whatever CC version is currently touching the file. flock-protected
// against concurrent writers.
func directWriteUserTurn(jsonlPath, sessionID, cwd, text string) error {
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

// waitForQuiet defers until the target JSONL has been stable for
// m.collisionWait, or aborts the wait at m.collisionMax. JSONL writes from
// the user's live `claude` process come in bursts (assistant streaming, tool
// chains); waiting for a quiet window minimizes the chance our headless
// inject interleaves mid-turn with their writes. Returns waited duration and
// whether we hit the timeout.
func (m *Mayor) waitForQuiet(jsonlPath string) (time.Duration, bool) {
	if m.collisionWait <= 0 || jsonlPath == "" {
		return 0, false
	}
	start := time.Now()
	var lastSize int64 = -1
	var stableSince time.Time
	for {
		info, err := os.Stat(jsonlPath)
		if err != nil {
			// missing JSONL = nothing to collide with; proceed.
			return time.Since(start), false
		}
		if info.Size() != lastSize {
			lastSize = info.Size()
			stableSince = time.Now()
		} else if time.Since(stableSince) >= m.collisionWait {
			return time.Since(start), false
		}
		if time.Since(start) >= m.collisionMax {
			return time.Since(start), true
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *Mayor) inject(sid, cwd, text string) error {
	cmd := exec.Command("claude", "--resume", sid, "--print", text)
	// `claude --resume <sid>` resolves the JSONL relative to cwd. Without
	// this, the resolver looks under the wrong project dir and fails with
	// "No conversation found". State.Cwd is recorded by the watcher from the
	// session's own JSONL events.
	if cwd != "" {
		cmd.Dir = cwd
	}
	// `--print` still reads stdin if attached to a tty; without /dev/null
	// the headless inject hangs ~3s waiting on user input, even though
	// `text` is already on the command line. See INJECTION.md gotchas.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("claude --resume --print: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  ↳ injected (cwd=%s), %d bytes assistant reply\n", cwd, len(out))
	return nil
}

// --- Phase 3: completion-report tracking ---

// trackInject records that we just sent text to target's live pane (via tmux
// or direct-write). The polling goroutine watches the JSONL for completion
// and speaks a Haiku summary when the chain quiesces. Headless inject path
// doesn't call this — completion is synchronous there, no follow-up needed.
//
// Re-tracking the same session overwrites the prior record. This is the
// right behavior for "user injected B before A finished": A's report is
// dropped (the user has moved on), and we now wait for B's completion.
func (m *Mayor) trackInject(target SessionEntry, text, narrate string) {
	if target.State.SessionID == "" || target.JSONLPath == "" {
		return
	}
	sz, _ := fileSize(target.JSONLPath)
	now := time.Now()
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	if m.pendingInjects == nil {
		m.pendingInjects = make(map[string]*pendingInject)
	}
	m.pendingInjects[target.State.SessionID] = &pendingInject{
		sessionID:          target.State.SessionID,
		project:            target.State.Project,
		jsonlPath:          target.JSONLPath,
		injectText:         text,
		injectTime:         now,
		sizeAtInject:       sz,
		lastSize:           sz,
		lastSizeChangeTime: now,
		narrate:            narrate,
	}
	m.recordRecentInject(target.State.SessionID, target.State.Project, text)
	go m.publishState()
}

// removePending deletes a pending inject under lock. Safe to call for a
// sessionID that's not present.
func (m *Mayor) removePending(sessionID string) {
	m.pendingMu.Lock()
	delete(m.pendingInjects, sessionID)
	m.pendingMu.Unlock()
	go m.publishState()
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// pollCompletions runs forever, ticking every 3 s and checking each pending
// inject for completion. Started in runAudioSock; not started in stdin mode
// (headless inject is synchronous, nothing to track).
func (m *Mayor) pollCompletions() {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for range tick.C {
		m.checkPendingInjects()
	}
}

func (m *Mayor) checkPendingInjects() {
	m.pendingMu.Lock()
	pending := make([]*pendingInject, 0, len(m.pendingInjects))
	for _, p := range m.pendingInjects {
		pending = append(pending, p)
	}
	m.pendingMu.Unlock()
	for _, p := range pending {
		m.checkOneInject(p)
	}
}

// checkOneInject is the per-session completion-detection state machine.
//
// Drop on TTL expiry — long-running tasks shouldn't block the slot forever
// in case our detector misses the completion signal.
//
// Drop on trivial growth — small tasks (one-line bash, status check) don't
// need a spoken report; the user heard the stock ack and that's enough.
//
// The completion signal: latest assistant block in the JSONL is `text`
// (no tool_use / thinking / tool_result trailing it) AND the JSONL has been
// size-stable for stabilityWindow. While a tool chain is running, the
// latest block is `tool_use` waiting for `tool_result` — this signal is
// genuinely off until the chain ends with an assistant text turn.
func (m *Mayor) checkOneInject(p *pendingInject) {
	now := time.Now()
	if now.Sub(p.injectTime) > m.completionTTL {
		fmt.Fprintf(os.Stderr, "  ↳ completion-tracker: TTL expired for %s, dropping\n", p.project)
		m.stopBlink(p, "", "", 0)
		m.removePending(p.sessionID)
		return
	}
	sz, err := fileSize(p.jsonlPath)
	if err != nil {
		return
	}
	if sz != p.lastSize {
		// JSONL grew — chain is still active. Reset the stability clock.
		m.pendingMu.Lock()
		p.lastSize = sz
		p.lastSizeChangeTime = now
		p.candidateFired = false
		m.pendingMu.Unlock()
		return
	}
	if p.candidateFired {
		return
	}
	if now.Sub(p.lastSizeChangeTime) < m.stabilityWindow {
		return
	}
	if now.Sub(p.injectTime) < m.minElapsed {
		return
	}
	if sz-p.sizeAtInject < m.minGrowthBytes && p.narrate != "force" {
		// Trivial — task barely produced output. Drop silently.
		// "force" narrate (user said "tell me…") bypasses this filter.
		fmt.Fprintf(os.Stderr, "  ↳ completion-tracker: trivial inject for %s (Δ%d bytes), dropping\n",
			p.project, sz-p.sizeAtInject)
		m.stopBlink(p, "", "", 0)
		m.removePending(p.sessionID)
		return
	}
	text, ready, err := assistantTextAfterInject(p.jsonlPath, p.sizeAtInject, p.injectText)
	if err != nil || !ready {
		return
	}
	m.pendingMu.Lock()
	p.candidateFired = true
	p.candidateText = text
	m.pendingMu.Unlock()
	go m.publishState()
	go m.fireCompletion(p)
}

// fireCompletion produces and speaks the completion report. Runs in its own
// goroutine so the Haiku call doesn't block the polling loop. The candidate
// text was captured by checkOneInject from the assistant block that
// followed our inject's echoed user-message in the JSONL — so it's
// definitely the answer to OUR inject, not whatever was at the JSONL tail
// when an inject queued behind unrelated work.
func (m *Mayor) fireCompletion(p *pendingInject) {
	defer m.removePending(p.sessionID)
	defer m.stopBlink(p, doneTag(p.project), "colour46", 2*time.Second)
	lastText := strings.TrimSpace(p.candidateText)
	if lastText == "" {
		fmt.Fprintf(os.Stderr, "  ↳ completion-tracker: empty candidate text for %s, skipping\n", p.project)
		return
	}
	summary, err := llm.RunSummarize(m.apiKey, m.cacheDir, p.injectText, lastText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ↳ completion-tracker: summarize failed for %s: %v\n", p.project, err)
		return
	}
	if strings.TrimSpace(summary) == "" {
		return
	}
	// V0.2.6: respect narrate policy. "silent" = log but don't speak.
	if p.narrate == "silent" {
		fmt.Fprintf(os.Stderr, "\033[2;32m✓ completion report\033[0m \033[2m(%s, silent)\033[0m: \033[2m%q\033[0m\n", p.project, summary)
		return
	}
	if err := m.audioWrite(map[string]any{"type": "speak", "text": summary}); err != nil {
		fmt.Fprintf(os.Stderr, "  ↳ completion-tracker: speak send failed: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "\033[1;32m✓ completion report\033[0m \033[2m(%s)\033[0m: \033[32m%q\033[0m\n", p.project, summary)
}

func defaultRuntimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return "/tmp"
}

// runClientWatchdog is V0.3.1's open-mic safety belt. When mayor is
// running inside a tmux session (saturday-stack), poll the client count
// every 10s; two consecutive zeros → send SIGUSR1 to the audio sidecar's
// pid (force-mute). Redundant with saturday-stack's client-detached tmux
// hook, but that hook can fail to fire (tmux server crash, weird session
// teardown, session started outside saturday-stack) and open mic without
// an attached client is the exact failure mode we're closing. SIGUSR1 on
// audio's side is asymmetric (mute-only, no unmute pair), so the operator
// still has to SPACEBAR re-arm after reattach — a stealth reattach can't
// silently restart capture.
func (m *Mayor) runClientWatchdog(pidfile string) {
	if os.Getenv("TMUX") == "" || pidfile == "" {
		return
	}
	sessionOut, err := exec.Command("tmux", "display-message", "-p", "#{session_name}").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\033[2m  client-watchdog: can't read tmux session name: %v (disabled)\033[0m\n", err)
		return
	}
	session := strings.TrimSpace(string(sessionOut))
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	zeros := 0
	muted := false
	for range tick.C {
		out, err := exec.Command("tmux", "list-clients", "-t", session).Output()
		if err != nil {
			return // session gone; nothing left to watch
		}
		clients := 0
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) != "" {
				clients++
			}
		}
		if clients == 0 {
			zeros++
			if zeros >= 2 && !muted {
				pidBytes, err := os.ReadFile(pidfile)
				if err != nil {
					continue
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
				if err != nil || pid <= 0 {
					continue
				}
				if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
					fmt.Fprintf(os.Stderr, "\033[2m  client-watchdog: SIGUSR1 pid %d failed: %v\033[0m\n", pid, err)
					continue
				}
				fmt.Fprintf(os.Stderr, "\033[1;33m  client-watchdog: 0 tmux clients — sent SIGUSR1 to audio pid %d\033[0m\n", pid)
				muted = true
			}
		} else {
			zeros = 0
			muted = false // reset so a future detach re-arms the SIGUSR1 send
		}
	}
}

// runArcRefresher is V0.2.7's slow-loop session-arc summarizer. Every
// arcInterval, fetches the watcher state, and for each active session with
// substantive content (LastUserTurn or LastAssistantText non-empty) calls
// llm.RunArc and stores the result in arcSummaries.
//
// Failures are logged dim and skipped — arc is best-effort context, never
// blocks expansion. Sessions that have aged out of the watcher (no longer in
// the snapshot) get their arc removed to bound map growth.
func (m *Mayor) runArcRefresher() {
	tick := time.NewTicker(m.arcInterval)
	defer tick.Stop()
	// Run once immediately on startup so a fresh mayor has arcs within
	// seconds rather than after the first interval. Sleep briefly first to
	// let the initial state-sock + audio-sock setup settle.
	time.Sleep(2 * time.Second)
	m.refreshArcs()
	for range tick.C {
		m.refreshArcs()
	}
}

func (m *Mayor) refreshArcs() {
	sessions, err := fetchSessions(m.sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\033[2m  arc-refresher: watcher fetch failed: %v\033[0m\n", err)
		return
	}
	live := map[string]struct{}{}
	for _, s := range sessions {
		if s.State.SessionID == "" {
			continue
		}
		live[s.State.SessionID] = struct{}{}
		summary, err := llm.RunArc(m.apiKey, m.cacheDir, s.State)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\033[2m  arc-refresher: %s: %v\033[0m\n", s.State.Project, err)
			continue
		}
		if summary == "" {
			continue
		}
		m.arcMu.Lock()
		prev := m.arcSummaries[s.State.SessionID]
		m.arcSummaries[s.State.SessionID] = summary
		m.arcMu.Unlock()
		if prev != summary {
			fmt.Fprintf(os.Stderr, "\033[2m  arc · %s · %s\033[0m\n", s.State.Project, summary)
		}
	}
	// Drop arcs for sessions no longer live so the map is bounded by
	// active-session count, not lifetime utterance count.
	m.arcMu.Lock()
	for sid := range m.arcSummaries {
		if _, ok := live[sid]; !ok {
			delete(m.arcSummaries, sid)
		}
	}
	m.arcMu.Unlock()
}

// enrichWithArc fills in s.SessionArc from the cached arc map before s is
// passed to the expander. No-op if no arc has been computed yet for this
// session — the expander's prompt makes the field optional.
func (m *Mayor) enrichWithArc(s *llm.State) {
	if s == nil || s.SessionID == "" {
		return
	}
	m.arcMu.Lock()
	defer m.arcMu.Unlock()
	if arc, ok := m.arcSummaries[s.SessionID]; ok && arc != "" {
		s.SessionArc = arc
	}
}

// assistantTextAfterInject scans the JSONL forward from fromOff and returns
// the text of the last assistant block that follows a user message
// containing injectText. ready=false means inject hasn't surfaced as a user
// message yet (still queued behind unrelated work) OR the most recent
// assistant block after our user-message is still tool_use/thinking
// (chain in progress) — caller should keep polling.
//
// The user-message gate is what makes this correct under "inject queued
// behind a previous task": pre-inject assistant text is ignored even if
// it's freshly written, because it predates our user message.
//
// CC stores actual user input as message.content="<string>"; tool_results
// arrive as content=[{"type":"tool_result",...}]. We only match string-form
// user content — tool_results never carry our injectText anyway.
//
// fromOff is sizeAtInject (the JSONL size when the inject went out). We
// rely on json.Valid to drop any partial line we land in mid-record.
func assistantTextAfterInject(jsonlPath string, fromOff int64, injectText string) (string, bool, error) {
	needle := strings.ToLower(strings.TrimSpace(injectText))
	if needle == "" {
		return "", false, nil
	}
	f, err := os.Open(jsonlPath)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	seekFrom := fromOff
	if seekFrom > info.Size() {
		return "", false, nil
	}
	if _, err := f.Seek(seekFrom, 0); err != nil {
		return "", false, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	userSeen := false
	var lastAssistantLine []byte
	for sc.Scan() {
		line := sc.Bytes()
		if !json.Valid(line) {
			continue
		}
		if bytes.Contains(line, []byte(`"type":"user"`)) {
			var ev struct {
				Type    string `json:"type"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &ev) != nil || ev.Type != "user" {
				continue
			}
			var s string
			if json.Unmarshal(ev.Message.Content, &s) != nil {
				continue // array form (tool_result), not human input
			}
			if strings.Contains(strings.ToLower(s), needle) {
				userSeen = true
				lastAssistantLine = nil
			}
			continue
		}
		if !userSeen {
			continue
		}
		if !bytes.Contains(line, []byte(`"type":"assistant"`)) {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		lastAssistantLine = cp
	}
	if err := sc.Err(); err != nil {
		return "", false, err
	}
	if !userSeen || lastAssistantLine == nil {
		return "", false, nil
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lastAssistantLine, &ev); err != nil {
		return "", false, err
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(ev.Message.Content, &blocks); err == nil && len(blocks) > 0 {
		last := blocks[len(blocks)-1]
		if last.Type == "text" {
			return last.Text, true, nil
		}
		return "", false, nil // tool_use / thinking — chain still running
	}
	var s string
	if err := json.Unmarshal(ev.Message.Content, &s); err == nil {
		return s, true, nil
	}
	return "", false, nil
}

// --- helpers ---

func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
}

// --- main ---

// xdgConfigHome returns $XDG_CONFIG_HOME (or $HOME/.config as fallback).
// Empty string only if $HOME is also unset, which doesn't happen on a sane
// Unix login.
func xdgConfigHome() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config")
	}
	return ""
}

// xdgCacheHome returns $XDG_CACHE_HOME (or $HOME/.cache as fallback).
func xdgCacheHome() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return x
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cache")
	}
	return ""
}

func main() {
	scriptDir := func() string {
		exe, err := os.Executable()
		if err == nil {
			return filepath.Dir(exe)
		}
		return "."
	}()
	if _, err := os.Stat(filepath.Join(scriptDir, "main.go")); err != nil {
		if wd, err := os.Getwd(); err == nil {
			scriptDir = wd
		}
	}

	// Default .env resolution: prefer XDG-standard ~/.config/saturday/config
	// if it exists; fall back to <scriptDir>/.env (in-repo dev).
	xdgEnv := ""
	if cfg := xdgConfigHome(); cfg != "" {
		xdgEnv = filepath.Join(cfg, "saturday", "config")
	}
	defaultEnv := filepath.Join(scriptDir, ".env")
	if xdgEnv != "" {
		if _, err := os.Stat(xdgEnv); err == nil {
			defaultEnv = xdgEnv
		}
	}

	// Default cache: XDG-standard ~/.cache/saturday/llm/ — keeps gitignored
	// LLM-response cache out of repo dirs entirely.
	defaultCache := filepath.Join(scriptDir, ".cache")
	if cacheBase := xdgCacheHome(); cacheBase != "" {
		defaultCache = filepath.Join(cacheBase, "saturday", "llm")
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	sock := flag.String("sock", "/tmp/saturday-watcher.sock", "watcher Unix socket path")
	envPath := flag.String("env", defaultEnv, ".env file with ANTHROPIC_API_KEY")
	cacheDir := flag.String("cache", defaultCache, "directory for cached LLM responses")
	dryRun := flag.Bool("dry-run", false, "log proposals but do not exec claude --resume --print")
	collisionWait := flag.Duration("collision-wait", 500*time.Millisecond, "JSONL must be size-stable for this long before injecting")
	collisionMax := flag.Duration("collision-max", 5*time.Second, "give up waiting and inject anyway after this")
	confThreshold := flag.Float64("conf-threshold", 0.5, "skip inject if router or expander confidence < this; 0 disables")
	injectDirectTokens := flag.Int("inject-direct-threshold", 80000, "if est. tokens since last isCompactSummary in target JSONL exceed this, skip headless `claude --resume --print` and write user turn directly to JSONL (let sync hook + live pane handle); 0 disables direct-write path. 80k is conservative for typical Sonnet/Opus context budgets — lower if you see autocompact-divert symptoms (e.g. mayor logs `injected, N bytes` but the assistant's reply is unrelated to the inject)")
	audioSock := flag.String("audio-sock", "", "if set, listen on this Unix socket for line-delimited JSON utterances from saturday-audio (V0.2 sidecar) instead of reading stdin. Empty = stdin mode.")
	cacheMax := flag.Int("cache-max", 1000, "max files in --cache dir; oldest by mtime are pruned on startup. Open-mic accumulates ~2 cache files per utterance (one route + one expand); 1000 ≈ a few weeks of normal use. 0 disables pruning.")
	stabilityWindow := flag.Duration("stability-window", 5*time.Second, "Phase 3: target JSONL must be size-stable for this long before considering a tracked inject complete")
	completionTTL := flag.Duration("completion-ttl", 10*time.Minute, "Phase 3: drop tracked-inject entries that haven't completed within this window")
	minGrowthBytes := flag.Int64("min-growth", 200, "Phase 3: skip completion report if JSONL grew less than this (in bytes) since inject — filters trivial one-line tasks")
	minElapsed := flag.Duration("min-elapsed", 5*time.Second, "Phase 3: skip completion report if less than this elapsed since inject — filters instant tasks where a spoken report would feel redundant after the stock ack")
	stateSock := flag.String("state-sock", "/tmp/saturday-mayor-state.sock", "V0.2.6: Unix socket exposing mayor cognitive state (line-delimited JSON snapshots, 1Hz heartbeat). Empty disables.")
	noBlink := flag.Bool("no-blink", false, "V0.2.6: disable das blinkenlights — the mid-height right-edge corner tag overlaid on the target CC pane during inject lifecycle.")
	hookSock := flag.String("hook-sock", "/tmp/saturday-mayor-hooks.sock", "V0.2.7: Unix socket receiving CC UserPromptSubmit/Stop events from the saturday-hook helper. One JSON line per accept. Empty disables.")
	arcInterval := flag.Duration("arc-interval", 5*time.Minute, "V0.2.7: cadence of slow-loop session-arc summarizer refresh per active session. 0 disables.")
	audioPidfile := flag.String("audio-pidfile", filepath.Join(defaultRuntimeDir(), "saturday-audio.pid"), "V0.3.1: pidfile written by saturday-audio; if set AND $TMUX is present, mayor polls tmux client count every 10s and sends SIGUSR1 to this pid when zero for two consecutive checks (force-mute the sidecar). Defense in depth against the tmux client-detached hook not firing. Empty disables.")
	askConf := flag.Float64("ask-conf", 0.7, "V0.3: classifier conf threshold to route an utterance to ask-mode (Saturday answers from arcs) instead of inject-mode (relayed to a CC session). Wake-word prefix bypasses this. Higher = stricter (fewer false-positive ask, more retypes).")
	flag.Parse()

	if *showVersion {
		fmt.Println(buildVersion())
		return
	}

	llm.LoadDotEnv(*envPath)
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY not set (checked env and "+*envPath+")")
		os.Exit(1)
	}
	if err := os.MkdirAll(*cacheDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir cache:", err)
		os.Exit(1)
	}
	if removed, err := llm.PruneLRU(*cacheDir, *cacheMax); err != nil {
		fmt.Fprintln(os.Stderr, "cache prune warning:", err)
	} else if removed > 0 {
		fmt.Fprintf(os.Stderr, "pruned %d stale cache entries (cap %d)\n", removed, *cacheMax)
	}

	m := &Mayor{
		apiKey:             apiKey,
		sockPath:           *sock,
		cacheDir:           *cacheDir,
		dryRun:             *dryRun,
		collisionWait:      *collisionWait,
		collisionMax:       *collisionMax,
		confThreshold:      *confThreshold,
		askConf:            *askConf,
		injectDirectTokens: *injectDirectTokens,
		stabilityWindow:    *stabilityWindow,
		completionTTL:      *completionTTL,
		minGrowthBytes:     *minGrowthBytes,
		minElapsed:         *minElapsed,
		startedAt:          time.Now(),
		state:              "idle",
		noBlink:            *noBlink,
		arcSummaries:       map[string]string{},
		arcInterval:        *arcInterval,
	}

	fmt.Fprintf(os.Stderr, "saturday-mayor — sock=%s dry-run=%v\n", *sock, *dryRun)

	if *stateSock != "" {
		go m.serveStateSock(*stateSock)
	}

	if *hookSock != "" {
		go m.serveHookSock(*hookSock)
	}

	if m.arcInterval > 0 {
		go m.runArcRefresher()
	}

	// V0.3.1 safety belt — force-mute audio if no tmux client is attached.
	// Self-disables when mayor isn't inside tmux or when --audio-pidfile="".
	go m.runClientWatchdog(*audioPidfile)

	if *audioSock != "" {
		runAudioSock(m, *audioSock)
	} else {
		runStdin(m)
	}
}

func runStdin(m *Mayor) {
	fmt.Fprintln(os.Stderr, "\033[1;32m[ready] saturday-mayor — stdin mode (type one utterance per line; ^D to exit)\033[0m")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := m.handle(line, "expand", "auto"); err != nil {
			fmt.Fprintf(os.Stderr, "\033[1;31m× %v\033[0m\n", err)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "stdin scan:", err)
		os.Exit(1)
	}
}

func runAudioSock(m *Mayor, sockPath string) {
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", sockPath, err)
		os.Exit(1)
	}
	defer os.Remove(sockPath)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		os.Remove(sockPath)
		os.Exit(0)
	}()

	go m.pollCompletions()

	fmt.Fprintf(os.Stderr, "\033[1;32m[ready] saturday-mayor — listening on %s for audio sidecar\033[0m\n", sockPath)
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "accept:", err)
			continue
		}
		fmt.Fprintln(os.Stderr, "audio sidecar connected")
		handleAudioConn(m, conn)
		fmt.Fprintln(os.Stderr, "audio sidecar disconnected")
	}
}

func handleAudioConn(m *Mayor, conn net.Conn) {
	m.audioMu.Lock()
	m.audioConn = conn
	m.audioMu.Unlock()
	defer func() {
		m.audioMu.Lock()
		m.audioConn = nil
		m.audioMu.Unlock()
		conn.Close()
	}()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev struct {
			Type    string  `json:"type"`
			Text    string  `json:"text"`
			Mode    string  `json:"mode"`    // "verbatim" or "expand"
			Narrate string  `json:"narrate"` // V0.2.6: "force"|"silent"|"auto" — speak Phase 3 summary policy
			Db      float64 `json:"db"`      // V0.2.7: rms frames carry dBFS here
			Ts      float64 `json:"ts"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			fmt.Fprintf(os.Stderr, "\033[1;31m× audio JSON: %v\033[0m: %s\n", err, line)
			continue
		}
		if ev.Type == "rms" {
			m.recordRMS(ev.Db)
			continue
		}
		if ev.Type != "utterance" {
			continue
		}
		text := strings.TrimSpace(ev.Text)
		if text == "" {
			continue
		}
		mode := ev.Mode
		if mode == "" {
			mode = "expand"
		}
		narrate := ev.Narrate
		if narrate == "" {
			narrate = "auto"
		}
		// V0.2.6: log incoming utterance prominently so the user doesn't
		// need to flip to the audio pane to see what was heard.
		fmt.Fprintf(os.Stderr, "\033[1;36m← utt\033[0m \033[2m(%s, narrate=%s)\033[0m %s\n",
			mode, narrate, text)
		if err := m.handle(text, mode, narrate); err != nil {
			fmt.Fprintf(os.Stderr, "\033[1;31m× %v\033[0m\n", err)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "audio sock scan:", err)
	}
}
