// Package orchestrator holds Saturday's ask/route/inject/summarize
// decision core, extracted from saturday-mayor so a second voice front
// end (saturday-voice) can reuse it instead of duplicating the
// classify/route/gate/inject/completion-tracking logic mayor's local-mic
// path already implements. saturday-mayor drives an Orchestrator itself
// now — this package's behavior is a pure refactor of what used to live
// directly on saturday-mayor's Mayor type, not a new design.
//
// Two things are deliberately NOT here, staying caller-specific:
//   - Presentation-layer state (mayor's state-sock / MayorState / the
//     das-blinkenlights corner tag's session bookkeeping beyond the
//     inject.Blinker value itself) — Handle returns a Decision the caller
//     can log to its own UI however it likes.
//   - Hook-sock (mayor's saturday-hook listener) itself — it stays
//     caller-side since it's about receiving local CC hook events, not
//     about voice orchestration. AccelerateCompletion and CheckRetype are
//     exported so a caller's own hook handler can still reach in.
package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"saturday/inject"
	llm "saturday/llmcore"
	"saturday/settle"
	"saturday/stageclient"
	"saturday/watcherclient"
)

// Config configures an Orchestrator. Zero-value-safe fields (Speak,
// EmitState, StageSock) simply disable the corresponding behavior — a
// caller that doesn't need window choreography or an activity spinner
// doesn't have to fake one.
type Config struct {
	APIKey             string
	CacheDir           string
	WatcherSock        string
	ConfThreshold      float64       // skip inject if router/expander confidence <= this; 0 disables
	AskConf            float64       // classifier conf >= this AND type==ask → ask-mode
	CollisionWait      time.Duration // JSONL must be size-stable this long before injecting
	CollisionMax       time.Duration // give up waiting and inject anyway after this
	InjectDirectTokens int           // est. tokens since last compact > this → direct-write skip headless
	StabilityWindow    time.Duration // Phase 3: JSONL must be size-stable this long before completion
	CompletionTTL      time.Duration // Phase 3: drop tracked injects that haven't completed within this
	MinGrowthBytes     int64         // Phase 3: skip completion report if JSONL grew less than this
	MinElapsed         time.Duration // Phase 3: skip completion report if less than this elapsed since inject
	ArcInterval        time.Duration // slow-loop arc-summarizer cadence; 0 disables
	NoBlink            bool          // disable the das-blinkenlights corner tag
	StageZoom          bool          // Posture A: zoom the addressed pane on focus
	StageTile          bool          // Posture A: salience-tile the addressed pane on focus
	StageSock          string        // if set, dial saturday-stage and send focus/restore commands
	DryRun             bool          // log proposals but don't actually commit an inject

	// Speak is called wherever the extracted logic used to write a
	// {"type":"speak","text":...} event to mayor's audio sidecar directly.
	// Wire it to your own TTS output. Nil = spoken replies are dropped
	// (logged to stderr only) — fine for a dry-run/test harness, not for
	// a real deployment.
	Speak func(text string) error

	// EmitState mirrors mayor's activity-spinner events ("routing",
	// "asking", "expanding", "injecting → <project>", "" = idle). Nil is a
	// safe no-op — a caller with no equivalent UI just leaves it unset.
	EmitState func(activity string)
}

// Decision is what Handle returns for an utterance the caller should log
// to its own presentation layer (e.g. mayor's state-sock recent-utterance
// ring). Nil means nothing to log — mirrors today's exact behavior of
// skipping recordUtterance on a confidence-gated skip.
type Decision struct {
	Mode  string // "expand" | "verbatim" | "ask"
	Route string // target project name, or "saturday" for ask-mode
	Conf  float64
}

// pendingInject tracks one outstanding inject awaiting a completion
// signal. Lifecycle: created in trackInject, removed in fireCompletion (on
// report) or checkOneInject (on TTL expiry / trivial-growth drop).
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
	candidateText      string
	narrate            string // "force" | "silent" | "auto"
	blinker            inject.Blinker
}

// RecentInjectRec is one entry in the expansion-feedback ring — see
// feedback.go. Exported so a caller's own hook handler (e.g. mayor's
// prompt_submit case) can log the matched record.
type RecentInjectRec struct {
	SessionID string
	Project   string
	Text      string
	TS        time.Time
}

// Orchestrator is the ask/route/inject/summarize decision core. The zero
// value is not usable — construct with New.
type Orchestrator struct {
	cfg   Config
	stage stageclient.Client

	pendingMu      sync.Mutex
	pendingInjects map[string]*pendingInject

	arcMu        sync.Mutex
	arcSummaries map[string]string

	// recentMu guards recent, the ask-context ring (≤10 formatted lines,
	// "text → route (mode)"). Deliberately separate from any presentation
	// ring a caller keeps from Handle's returned Decision — this one
	// exists only to feed llm.AskContext.RecentUtterances.
	recentMu sync.Mutex
	recent   []string

	recentInjectsMu sync.Mutex
	recentInjects   []RecentInjectRec
}

// New constructs an Orchestrator and starts its background goroutines:
// the arc summarizer (if cfg.ArcInterval > 0) and the stage sidecar dialer
// (if cfg.StageSock != ""). It does NOT start completion polling — call
// StartCompletionPolling explicitly, matching mayor's existing behavior of
// only polling in audio-sock mode, not stdin mode.
func New(cfg Config) *Orchestrator {
	o := &Orchestrator{
		cfg:            cfg,
		pendingInjects: map[string]*pendingInject{},
		arcSummaries:   map[string]string{},
	}
	if cfg.ArcInterval > 0 {
		go o.runArcRefresher()
	}
	if cfg.StageSock != "" {
		go o.stage.Run(cfg.StageSock)
	}
	return o
}

// StartCompletionPolling launches the background ticker that checks
// pending injects for completion every 3s. Separate from New so a caller
// whose transport already gets a reliable completion signal some other
// way (e.g. a hook Stop event via AccelerateCompletion) can skip it —
// mirrors mayor's conditional start in its audio-sock mode.
func (o *Orchestrator) StartCompletionPolling() {
	go o.pollCompletions()
}

func (o *Orchestrator) speak(text string) {
	if o.cfg.Speak == nil {
		return
	}
	if err := o.cfg.Speak(text); err != nil {
		fmt.Fprintf(os.Stderr, "  ↳ speak failed: %v\n", err)
	}
}

func (o *Orchestrator) emitState(activity string) {
	if o.cfg.EmitState != nil {
		o.cfg.EmitState(activity)
	}
}

func (o *Orchestrator) recordRecentUtterance(formatted string) {
	o.recentMu.Lock()
	o.recent = append(o.recent, formatted)
	if len(o.recent) > 10 {
		o.recent = o.recent[len(o.recent)-10:]
	}
	o.recentMu.Unlock()
}

// stripWakeWord detects a leading "saturday" / "hey saturday" wake word
// and returns the utterance with the prefix removed plus a true flag. The
// follow-on character must be whitespace or end-of-string punctuation so
// "saturdayfile.go" doesn't accidentally trigger. Bare "saturday" (nothing
// after) returns "" + true — caller routes to ask-mode with a generic
// "what's on" probe.
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

func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Handle runs one utterance through the pipeline: classify (ask vs.
// inject), then either answerAsk or route/expand/inject. mode is "expand"
// or "verbatim" (verbatim skips the LLM expander — utterance text becomes
// the inject directly; router still picks the session). narrate is
// "force"|"silent"|"auto" — controls whether a Phase 3 spoken completion
// summary fires.
//
// Returns a non-nil Decision for any utterance the caller should log to
// its own presentation layer; nil means nothing to log (e.g. a
// confidence-gated skip, or a fetch/router error).
func (o *Orchestrator) Handle(utterance, mode, narrate string) (*Decision, error) {
	if mode != "verbatim" {
		if cleaned, isAsk := stripWakeWord(utterance); isAsk {
			fmt.Fprintf(os.Stderr, "\033[35m? ask\033[0m \033[2m(wake-word)\033[0m\n")
			return o.answerAsk(cleaned)
		}
		// Classifier — Haiku call, ~$0.0001 per utterance. Errors fall
		// through silently to inject; classifier is a UX optimization, not
		// a load-bearing decision.
		t, conf, rat, err := llm.RunClassify(o.cfg.APIKey, o.cfg.CacheDir, utterance)
		if err == nil {
			if t == "ask" && conf >= o.cfg.AskConf {
				fmt.Fprintf(os.Stderr, "\033[35m? ask\033[0m \033[2m(conf=%.2f — %s)\033[0m\n",
					conf, oneLine(rat))
				return o.answerAsk(utterance)
			}
			if t == "ask" {
				fmt.Fprintf(os.Stderr, "  \033[2m↳ classifier ask conf=%.2f below %.2f; treating as inject\033[0m\n",
					conf, o.cfg.AskConf)
			}
		}
	}

	o.emitState("routing")
	defer o.emitState("")
	sessions, err := watcherclient.FetchSessions(o.cfg.WatcherSock)
	if err != nil {
		return nil, fmt.Errorf("fetch sessions: %w", err)
	}
	live := make([]watcherclient.SessionEntry, 0, len(sessions))
	for _, s := range sessions {
		if s.State.SessionID != "" {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return nil, errors.New("no active sessions in watcher state")
	}
	if len(live) == 1 {
		// Single-session shortcut: skip the router, expand against the
		// only target.
		dec := &Decision{Mode: mode, Route: live[0].State.Project, Conf: 0}
		o.recordRecentUtterance(fmt.Sprintf("%s → %s (%s)", oneLine(utterance), dec.Route, dec.Mode))
		return dec, o.expandAndInject(utterance, live[0], mode, narrate)
	}

	cands := make([]llm.State, len(live))
	for i, s := range live {
		cands[i] = s.State
		// Enrich routing candidates with the cached arc summary so the
		// router can disambiguate anaphoric references ("rerun it", "the
		// same one") by session theme, not just last-N-turn signals.
		o.enrichWithArc(&cands[i])
	}
	rt, err := llm.RunRoute(o.cfg.APIKey, o.cfg.CacheDir, utterance, cands)
	if err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}
	idx, ok := getInt(rt, "target_index")
	if !ok || idx < 0 || idx >= len(live) {
		return nil, fmt.Errorf("router returned bad target_index: %v", rt["target_index"])
	}
	conf := getFloat(rt, "confidence")
	target := live[idx]
	fmt.Fprintf(os.Stderr, "\033[2;36m→ route:\033[0m %s \033[2m(conf=%.2f)\033[0m \033[2m— %s\033[0m\n",
		target.State.Project, conf, oneLine(getStr(rt, "rationale")))
	if o.cfg.ConfThreshold > 0 && conf <= o.cfg.ConfThreshold {
		fmt.Fprintf(os.Stderr, "  ↳ router conf below threshold %.2f; skipping inject\n", o.cfg.ConfThreshold)
		return nil, nil
	}
	dec := &Decision{Mode: mode, Route: target.State.Project, Conf: conf}
	o.recordRecentUtterance(fmt.Sprintf("%s → %s (%s)", oneLine(utterance), dec.Route, dec.Mode))
	return dec, o.expandAndInject(utterance, target, mode, narrate)
}

// answerAsk is the ask-mode path: gather Saturday's bird's-eye state
// (arcs, recent utterances, in-flight injects) and call llm.RunAsk to
// produce a brief spoken answer. No pendingInject is created — ask is a
// terminal action.
func (o *Orchestrator) answerAsk(utterance string) (*Decision, error) {
	o.emitState("asking")
	defer o.emitState("")

	if utterance == "" {
		// Bare wake word ("saturday") with no follow-on — treat as a
		// generic "what's on" probe rather than failing.
		utterance = "what's on"
	}

	ctx := llm.AskContext{
		Arcs:         map[string]string{},
		ProjectBySID: map[string]string{},
	}

	if sessions, err := watcherclient.FetchSessions(o.cfg.WatcherSock); err == nil {
		o.arcMu.Lock()
		for _, s := range sessions {
			sid := s.State.SessionID
			if sid == "" {
				continue
			}
			if arc, ok := o.arcSummaries[sid]; ok && arc != "" {
				ctx.Arcs[sid] = arc
				ctx.ProjectBySID[sid] = s.State.Project
			}
		}
		o.arcMu.Unlock()
	}

	o.recentMu.Lock()
	ctx.RecentUtterances = append([]string{}, o.recent...)
	o.recentMu.Unlock()

	o.pendingMu.Lock()
	for _, p := range o.pendingInjects {
		ctx.TrackedInjects = append(ctx.TrackedInjects,
			fmt.Sprintf("%s: %s", p.project, oneLine(head(p.injectText, 80))))
	}
	o.pendingMu.Unlock()

	reply, err := llm.RunAsk(o.cfg.APIKey, o.cfg.CacheDir, utterance, ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\033[31m× ask:\033[0m %v\n", err)
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "\033[35m? ask:\033[0m %q\n  \033[35m→\033[0m %s\n",
		oneLine(utterance), reply)

	o.recordRecentUtterance(fmt.Sprintf("%s → saturday (ask)", oneLine(utterance)))
	o.speak(reply)
	return &Decision{Mode: "ask", Route: "saturday", Conf: 1.0}, nil
}

// commitInject runs the inject-execution path: collision-wait, optional
// narration, path selection (tmux → direct-write → headless). Used by
// both expand-mode (after the expander returns action=inject) and
// verbatim-mode (utterance text becomes the inject directly, narration
// empty).
func (o *Orchestrator) commitInject(target watcherclient.SessionEntry, text, narration, narrate string) error {
	o.emitState("injecting → " + target.State.Project)
	if o.cfg.DryRun {
		fmt.Fprintln(os.Stderr, "  [dry-run; skipping exec]")
		return nil
	}
	if narration != "" {
		o.speak(narration)
		fmt.Fprintf(os.Stderr, "  ↳ narrating: %q\n", narration)
	}
	waited, timedOut := settle.WaitForQuiet(target.JSONLPath, o.cfg.CollisionWait, o.cfg.CollisionMax)
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
	if paneID := inject.FindTmuxPane(target.State.Cwd); paneID != "" {
		fmt.Fprintf(os.Stderr, "  ↳ found tmux pane %s for cwd=%s; using tmux send-keys\n", paneID, target.State.Cwd)
		if err := inject.ViaTmux(paneID, text); err != nil {
			return fmt.Errorf("tmux send-keys: %w", err)
		}
		fmt.Fprintln(os.Stderr, "  ↳ injected via tmux send-keys (live pane handles)")
		o.trackInject(target, text, narrate)
		o.startBlink(o.getPending(target.State.SessionID), paneID)
		// Window choreography: surface + highlight the addressed pane. Only
		// reached after the inject committed (post confThreshold), so this
		// is confident by construction.
		o.stage.Write(map[string]any{
			"type":       "focus",
			"session_id": target.State.SessionID,
			"project":    target.State.Project,
			"pane_id":    paneID,
			"cwd":        target.State.Cwd,
			"zoom":       o.cfg.StageZoom,
			"tile":       o.cfg.StageTile,
		})
		return nil
	}
	fmt.Fprintln(os.Stderr, "  ↳ no tmux pane found for target cwd; using JSONL fallback path")
	if o.cfg.InjectDirectTokens > 0 && target.JSONLPath != "" {
		est, err := inject.TokensSinceLastCompact(target.JSONLPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ↳ token-estimate failed: %v; falling back to headless inject\n", err)
		} else if est > o.cfg.InjectDirectTokens {
			fmt.Fprintf(os.Stderr, "  ↳ post-compact tokens-est %d > %d threshold; direct-writing user turn (no headless invocation)\n",
				est, o.cfg.InjectDirectTokens)
			if err := inject.DirectWriteUserTurn(target.JSONLPath, target.State.SessionID, target.State.Cwd, text); err != nil {
				return fmt.Errorf("direct-write: %w", err)
			}
			fmt.Fprintf(os.Stderr, "  ↳ direct-wrote user turn to %s\n", target.JSONLPath)
			o.trackInject(target, text, narrate)
			return nil
		} else {
			fmt.Fprintf(os.Stderr, "  ↳ post-compact tokens-est %d ≤ %d; using headless inject\n",
				est, o.cfg.InjectDirectTokens)
		}
	}
	n, err := inject.Headless(target.State.SessionID, target.State.Cwd, text)
	if err != nil {
		return fmt.Errorf("claude --resume --print: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  ↳ injected (cwd=%s), %d bytes assistant reply\n", target.State.Cwd, n)
	return nil
}

func (o *Orchestrator) expandAndInject(utterance string, target watcherclient.SessionEntry, mode, narrate string) error {
	if mode == "verbatim" {
		// Verbatim mode: utterance text becomes the inject directly. No
		// expander LLM call, no narration speak event — the caller's
		// instant stock ack is the audible feedback.
		fmt.Fprintf(os.Stderr, "\033[1;33m→ Saturday → %s\033[0m \033[2m(verbatim)\033[0m: \033[33m%s\033[0m\n",
			target.State.Project, oneLine(utterance))
		return o.commitInject(target, utterance, "", narrate)
	}
	o.emitState("expanding")
	o.enrichWithArc(&target.State)
	exp, err := llm.RunExpand(o.cfg.APIKey, o.cfg.CacheDir, utterance, target.State)
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
		if o.cfg.ConfThreshold > 0 && conf <= o.cfg.ConfThreshold {
			fmt.Fprintf(os.Stderr, "  ↳ expander conf below threshold %.2f; skipping inject\n", o.cfg.ConfThreshold)
			return nil
		}
		text = inject.WithCallsignRule(text)
		return o.commitInject(target, text, getStr(exp, "confirmation"), narrate)
	case "ask":
		fmt.Fprintf(os.Stderr, "\033[1;35m? expander asks\033[0m \033[2m(%s)\033[0m: %s\n", target.State.Project, oneLine(text))
		o.speak(text)
		return nil
	case "decline":
		fmt.Fprintf(os.Stderr, "\033[1;31m✗ expander declined\033[0m \033[2m(%s)\033[0m: \033[2m%s\033[0m\n", target.State.Project, oneLine(getStr(exp, "rationale")))
		return nil
	default:
		return fmt.Errorf("expander returned unknown action: %q", action)
	}
}

// --- Phase 3: completion-report tracking ---

// trackInject records that we just sent text to target's live pane (via
// tmux or direct-write). The polling loop (or a caller's own
// AccelerateCompletion call) watches for completion and speaks a summary
// when the chain quiesces. Headless inject doesn't call this — completion
// is synchronous there, no follow-up needed.
//
// Re-tracking the same session overwrites the prior record: "user
// injected B before A finished" drops A's report (the user has moved on)
// and waits for B's completion instead.
func (o *Orchestrator) trackInject(target watcherclient.SessionEntry, text, narrate string) {
	if target.State.SessionID == "" || target.JSONLPath == "" {
		return
	}
	sz, _ := settle.FileSize(target.JSONLPath)
	now := time.Now()
	o.pendingMu.Lock()
	o.pendingInjects[target.State.SessionID] = &pendingInject{
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
	o.pendingMu.Unlock()
	o.recordRecentInject(target.State.SessionID, target.State.Project, text)
}

// removePending deletes a pending inject. Safe to call for a sessionID
// that's not present.
func (o *Orchestrator) removePending(sessionID string) {
	o.pendingMu.Lock()
	proj := ""
	if p := o.pendingInjects[sessionID]; p != nil {
		proj = p.project
	}
	delete(o.pendingInjects, sessionID)
	o.pendingMu.Unlock()
	// De-emphasize the addressed window on any teardown (completion, TTL
	// expiry, interruption). No-op for sessions stage never highlighted.
	o.stage.Write(map[string]any{"type": "restore", "session_id": sessionID, "project": proj})
}

// getPending fetches a pending inject by sessionID. Returns nil if not
// present.
func (o *Orchestrator) getPending(sessionID string) *pendingInject {
	o.pendingMu.Lock()
	defer o.pendingMu.Unlock()
	return o.pendingInjects[sessionID]
}

// PendingInject is a read-only snapshot of one tracked, not-yet-completed
// inject — for a caller's own presentation layer (e.g. mayor's state-sock
// "tracked" field).
type PendingInject struct {
	SessionID      string
	Project        string
	InjectTime     time.Time
	CandidateFired bool
}

// PendingInjects returns a snapshot of every currently tracked inject.
func (o *Orchestrator) PendingInjects() []PendingInject {
	o.pendingMu.Lock()
	defer o.pendingMu.Unlock()
	out := make([]PendingInject, 0, len(o.pendingInjects))
	for _, p := range o.pendingInjects {
		out = append(out, PendingInject{
			SessionID:      p.sessionID,
			Project:        p.project,
			InjectTime:     p.injectTime,
			CandidateFired: p.candidateFired,
		})
	}
	return out
}

// AccelerateCompletion short-circuits the JSONL-stability poll for a
// tracked inject — for a caller with its own authoritative "turn done"
// signal (e.g. mayor's saturday-hook Stop event) that arrives faster than
// the 3s poll tick would notice. Returns the tracked inject's project and
// true if one was found; ok=false (project="") if sessionID has no
// tracked inject — a no-op in that case.
func (o *Orchestrator) AccelerateCompletion(sessionID string) (project string, ok bool) {
	o.pendingMu.Lock()
	p, found := o.pendingInjects[sessionID]
	o.pendingMu.Unlock()
	if !found {
		return "", false
	}
	p.lastSizeChangeTime = time.Now().Add(-o.cfg.StabilityWindow - time.Second)
	o.checkOneInject(p)
	return p.project, true
}

// pollCompletions runs forever, ticking every 3s and checking each
// pending inject for completion. Only useful if started — see
// StartCompletionPolling.
func (o *Orchestrator) pollCompletions() {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for range tick.C {
		o.checkPendingInjects()
	}
}

func (o *Orchestrator) checkPendingInjects() {
	o.pendingMu.Lock()
	pending := make([]*pendingInject, 0, len(o.pendingInjects))
	for _, p := range o.pendingInjects {
		pending = append(pending, p)
	}
	o.pendingMu.Unlock()
	for _, p := range pending {
		o.checkOneInject(p)
	}
}

// checkOneInject is the per-session completion-detection state machine.
//
// Drop on TTL expiry — long-running tasks shouldn't block the slot
// forever in case the detector misses the completion signal.
//
// Drop on trivial growth — small tasks (one-line bash, status check)
// don't need a spoken report; the stock ack was enough.
//
// The completion signal: latest assistant block in the JSONL is `text`
// (no trailing tool_use/thinking/tool_result) AND the JSONL has been
// size-stable for StabilityWindow.
func (o *Orchestrator) checkOneInject(p *pendingInject) {
	now := time.Now()
	if now.Sub(p.injectTime) > o.cfg.CompletionTTL {
		fmt.Fprintf(os.Stderr, "  ↳ completion-tracker: TTL expired for %s, dropping\n", p.project)
		o.stopBlink(p, "", "", 0)
		o.removePending(p.sessionID)
		return
	}
	sz, err := settle.FileSize(p.jsonlPath)
	if err != nil {
		return
	}
	if sz != p.lastSize {
		// JSONL grew — chain is still active. Reset the stability clock.
		o.pendingMu.Lock()
		p.lastSize = sz
		p.lastSizeChangeTime = now
		p.candidateFired = false
		o.pendingMu.Unlock()
		return
	}
	if p.candidateFired {
		return
	}
	if now.Sub(p.lastSizeChangeTime) < o.cfg.StabilityWindow {
		return
	}
	if now.Sub(p.injectTime) < o.cfg.MinElapsed {
		return
	}
	if sz-p.sizeAtInject < o.cfg.MinGrowthBytes && p.narrate != "force" {
		// Trivial — task barely produced output. Drop silently. "force"
		// narrate (user said "tell me…") bypasses this filter.
		fmt.Fprintf(os.Stderr, "  ↳ completion-tracker: trivial inject for %s (Δ%d bytes), dropping\n",
			p.project, sz-p.sizeAtInject)
		o.stopBlink(p, "", "", 0)
		o.removePending(p.sessionID)
		return
	}
	text, ready, err := settle.AssistantTextAfterInject(p.jsonlPath, p.sizeAtInject, p.injectText)
	if err != nil || !ready {
		return
	}
	o.pendingMu.Lock()
	p.candidateFired = true
	p.candidateText = text
	o.pendingMu.Unlock()
	go o.fireCompletion(p)
}

// fireCompletion produces and speaks the completion report. Runs in its
// own goroutine so the summarize call doesn't block the polling loop. The
// candidate text was captured by checkOneInject from the assistant block
// that followed our inject's echoed user-message in the JSONL — so it's
// definitely the answer to OUR inject, not whatever was at the JSONL tail
// when an inject queued behind unrelated work.
func (o *Orchestrator) fireCompletion(p *pendingInject) {
	defer o.removePending(p.sessionID)
	defer o.stopBlink(p, inject.DoneTag(p.project), "colour46", 2*time.Second)
	lastText := strings.TrimSpace(p.candidateText)
	if lastText == "" {
		fmt.Fprintf(os.Stderr, "  ↳ completion-tracker: empty candidate text for %s, skipping\n", p.project)
		return
	}
	summary, err := llm.RunSummarize(o.cfg.APIKey, o.cfg.CacheDir, p.injectText, lastText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ↳ completion-tracker: summarize failed for %s: %v\n", p.project, err)
		return
	}
	if strings.TrimSpace(summary) == "" {
		return
	}
	if p.narrate == "silent" {
		fmt.Fprintf(os.Stderr, "\033[2;32m✓ completion report\033[0m \033[2m(%s, silent)\033[0m: \033[2m%q\033[0m\n", p.project, summary)
		return
	}
	o.speak(summary)
	fmt.Fprintf(os.Stderr, "\033[1;32m✓ completion report\033[0m \033[2m(%s)\033[0m: \033[32m%q\033[0m\n", p.project, summary)
}

// --- das blinkenlights (corner tag) ---
//
// Mechanics (pane discovery, status-right get/set/restore) live in
// saturday/inject; these are thin wrappers that hold the per-inject
// inject.Blinker value under pendingMu.

func (o *Orchestrator) startBlink(p *pendingInject, paneID string) {
	if p == nil {
		return
	}
	b := inject.StartBlink(paneID, p.project, o.cfg.NoBlink)
	o.pendingMu.Lock()
	p.blinker = b
	o.pendingMu.Unlock()
}

func (o *Orchestrator) stopBlink(p *pendingInject, finalText, finalColor string, fadeAfter time.Duration) {
	if p == nil {
		return
	}
	o.pendingMu.Lock()
	b := p.blinker
	p.blinker = inject.Blinker{}
	o.pendingMu.Unlock()
	b.Stop(finalText, finalColor, fadeAfter, o.cfg.NoBlink)
}

// --- slow-loop session-arc summarizer ---

// runArcRefresher fetches watcher state every ArcInterval and, for each
// active session with substantive content, calls llm.RunArc and stores
// the result. Failures are logged dim and skipped — arc is best-effort
// context, never blocks expansion.
func (o *Orchestrator) runArcRefresher() {
	tick := time.NewTicker(o.cfg.ArcInterval)
	defer tick.Stop()
	// Run once immediately so a fresh Orchestrator has arcs within
	// seconds rather than after the first interval.
	time.Sleep(2 * time.Second)
	o.refreshArcs()
	for range tick.C {
		o.refreshArcs()
	}
}

func (o *Orchestrator) refreshArcs() {
	sessions, err := watcherclient.FetchSessions(o.cfg.WatcherSock)
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
		summary, err := llm.RunArc(o.cfg.APIKey, o.cfg.CacheDir, s.State)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\033[2m  arc-refresher: %s: %v\033[0m\n", s.State.Project, err)
			continue
		}
		if summary == "" {
			continue
		}
		o.arcMu.Lock()
		prev := o.arcSummaries[s.State.SessionID]
		o.arcSummaries[s.State.SessionID] = summary
		o.arcMu.Unlock()
		if prev != summary {
			fmt.Fprintf(os.Stderr, "\033[2m  arc · %s · %s\033[0m\n", s.State.Project, summary)
		}
	}
	// Drop arcs for sessions no longer live so the map is bounded by
	// active-session count, not lifetime utterance count.
	o.arcMu.Lock()
	for sid := range o.arcSummaries {
		if _, ok := live[sid]; !ok {
			delete(o.arcSummaries, sid)
		}
	}
	o.arcMu.Unlock()
}

// enrichWithArc fills in s.SessionArc from the cached arc map. No-op if no
// arc has been computed yet for this session.
func (o *Orchestrator) enrichWithArc(s *llm.State) {
	if s == nil || s.SessionID == "" {
		return
	}
	o.arcMu.Lock()
	defer o.arcMu.Unlock()
	if arc, ok := o.arcSummaries[s.SessionID]; ok && arc != "" {
		s.SessionArc = arc
	}
}
