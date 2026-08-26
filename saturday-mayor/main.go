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
// The classify/ask/route/expand/inject/summarize decision core lives in
// the saturday/orchestrator package (extracted so saturday-voice can
// reuse it) — this file drives an orchestrator.Orchestrator and owns
// everything specific to mayor's own local-mic surface: the audio
// sidecar connection, the cognitive-state socket (thinking-pane UI), the
// das-blinkenlights session bookkeeping's caller side, and the
// saturday-hook listener.
//
// One mayor process per user. Sequential pipeline — one utterance at a
// time. JSONL-write serialization is implicit because injects don't
// overlap. Concurrent stdin lines are processed FIFO.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
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
	"saturday/orchestrator"
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

// --- Pipeline ---

type Mayor struct {
	orch *orchestrator.Orchestrator

	// V0.2.1 audio sidecar back-writes. audioMu serializes writes to audioConn
	// so concurrent writers (pipeline narration + state events + Phase 3
	// completion reports from the polling goroutine) don't interleave bytes.
	audioMu   sync.Mutex
	audioConn net.Conn

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

// speak wires orchestrator.Config.Speak — a spoken reply from the
// decision core becomes a {"type":"speak",...} event on the audio
// sidecar, exactly as the pre-extraction code wrote it inline.
//
// Every spoken reply, regardless of source (summarizer, ask-mode, expander
// confirmation), funnels through here — the single choke point to score
// against the voice register rather than wiring cope-gate into each
// llmcore generator separately. See llm.CheckVoiceRegister.
func (m *Mayor) speak(text string) error {
	go llm.CheckVoiceRegister(text)
	return m.audioWrite(map[string]any{"type": "speak", "text": text})
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

	now := time.Now()
	pending := m.orch.PendingInjects()
	tracked := make([]TrackedInject, 0, len(pending))
	for _, p := range pending {
		block := "running"
		if p.CandidateFired {
			block = "text"
		}
		tracked = append(tracked, TrackedInject{
			SID:   p.SessionID,
			Proj:  p.Project,
			AgeS:  now.Sub(p.InjectTime).Seconds(),
			Block: block,
		})
	}

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
// lifetime turn counter, then broadcasts. Called once per utterance for
// which orchestrator.Handle returned a non-nil Decision.
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

// dispatch runs one utterance through the orchestrator and mirrors its
// Decision (if any) into mayor's own state-sock recent-utterance ring —
// the same recordUtterance call the pre-extraction handle() made inline,
// now driven by what Handle reports back.
func (m *Mayor) dispatch(utterance, mode, narrate string) error {
	dec, err := m.orch.Handle(utterance, mode, narrate, nil)
	if dec != nil {
		m.recordUtterance(utterance, dec.Mode, dec.Route, dec.Conf)
	}
	return err
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
	if err := os.Chmod(sockPath, 0o600); err != nil {
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
		// likely wrong / late / swallowed. CheckRetype logs the match to
		// the feedback JSONL itself; we just surface it here.
		if rec, sim, isRetype := m.orch.CheckRetype(sid, prompt); isRetype {
			age := time.Since(rec.TS)
			fmt.Fprintf(os.Stderr,
				"\033[35m  feedback · retype\033[0m · %s · sim=%.2f · %s ago\n  \033[2minject:\033[0m %q\n  \033[2mtyped:\033[0m  %q\n",
				rec.Project, sim, age.Round(time.Second), oneLine(head(rec.Text, 80)), oneLine(head(prompt, 80)))
		}
	case "stop":
		// If we have a pendingInject for this session, fire Phase 3
		// immediately. Stability poll was the JSONL-only proxy for "turn
		// done"; the Stop hook is the authoritative signal.
		project, ok := m.orch.AccelerateCompletion(sid)
		if !ok {
			fmt.Fprintf(os.Stderr, "\033[2m  hook · stop · %s · (no tracked inject)\033[0m\n", head(sid, 8))
			return
		}
		fmt.Fprintf(os.Stderr, "\033[2m  hook · stop · %s · %s · firing Phase 3\033[0m\n",
			head(sid, 8), project)
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

func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
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
	if err := os.Chmod(sockPath, 0o600); err != nil {
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
	confThreshold := flag.Float64("conf-threshold", 0.5, "skip inject if router or expander confidence <= this (must exceed to proceed); 0 disables")
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
	stageSock := flag.String("stage-sock", "", "if set, dial this Unix socket and send focus/restore commands to the saturday-stage window-choreography sidecar on the inject lifecycle. Empty = disabled (no window choreography).")
	stageZoom := flag.Bool("stage-zoom", false, "Posture A (cockpit): on inject, ask stage to zoom (maximize) the addressed pane; restore unzooms. Takes precedence over --stage-tile.")
	stageTile := flag.Bool("stage-tile", false, "Posture A (cockpit): on inject, ask stage to give the addressed pane a proportionally larger share of an even-horizontal row (salience tiling); restore re-evens.")
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
		startedAt: time.Now(),
		state:     "idle",
	}

	m.orch = orchestrator.New(orchestrator.Config{
		APIKey:             apiKey,
		CacheDir:           *cacheDir,
		WatcherSock:        *sock,
		ConfThreshold:      *confThreshold,
		AskConf:            *askConf,
		CollisionWait:      *collisionWait,
		CollisionMax:       *collisionMax,
		InjectDirectTokens: *injectDirectTokens,
		StabilityWindow:    *stabilityWindow,
		CompletionTTL:      *completionTTL,
		MinGrowthBytes:     *minGrowthBytes,
		MinElapsed:         *minElapsed,
		ArcInterval:        *arcInterval,
		NoBlink:            *noBlink,
		StageZoom:          *stageZoom,
		StageTile:          *stageTile,
		StageSock:          *stageSock,
		DryRun:             *dryRun,
		Speak:              m.speak,
		EmitState:          m.emitState,
	})

	fmt.Fprintf(os.Stderr, "saturday-mayor — sock=%s dry-run=%v\n", *sock, *dryRun)

	if *stateSock != "" {
		go m.serveStateSock(*stateSock)
	}

	if *hookSock != "" {
		go m.serveHookSock(*hookSock)
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
		if err := m.dispatch(line, "expand", "auto"); err != nil {
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

	m.orch.StartCompletionPolling()

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
		if err := m.dispatch(text, mode, narrate); err != nil {
			fmt.Fprintf(os.Stderr, "\033[1;31m× %v\033[0m\n", err)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "audio sock scan:", err)
	}
}
