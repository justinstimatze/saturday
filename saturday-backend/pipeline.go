package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"saturday/inject"
	llm "saturday/llmcore"
	"saturday/settle"
	"saturday/stageclient"
	"saturday/watcherclient"
)

// backend holds the config processNote and commitInject need. The poll
// loop in main.go constructs one and calls processNote per note.
type backend struct {
	apiKey             string
	sockPath           string
	cacheDir           string
	dryRun             bool
	collisionWait      time.Duration
	collisionMax       time.Duration
	confThreshold      float64
	injectDirectTokens int

	// saturday-stage window-choreography sidecar, shared with saturday-mayor
	// via the stageclient package — same dial/write logic, so a phone-voice
	// inject resizes the pane the same way a local-mic one already does.
	// Zero-value stage permanently no-ops if --stage-sock was never set,
	// same resilience posture as mayor's.
	stage               stageclient.Client
	stageZoom           bool
	stageTile           bool
	stageRestorePoll    time.Duration
	stageRestoreMaxWait time.Duration
}

// belowThreshold reports whether conf fails to clear threshold. A threshold
// of 0 disables the gate. Exact equality fails the gate — a router/expander
// score sitting right at the default 0.50 is a coin flip, not a pass, which
// is what let a stale test note force-route onto the only live session
// during Phase 1's first live run.
func belowThreshold(conf, threshold float64) bool {
	return threshold > 0 && conf <= threshold
}

// processNote routes note text to the right live session and injects it —
// the same router/expander pipeline saturday-mayor's handle()/
// expandAndInject() run for voice utterances, minus the wake-word/
// classifier ask-mode branch: per the tested Drive-relay rule, a note only
// gets written when there's a named session and a task, so every note is
// an inject-mode request by construction, never an ask.
func (b *backend) processNote(text string) error {
	sessions, err := watcherclient.FetchSessions(b.sockPath)
	if err != nil {
		return fmt.Errorf("fetch sessions: %w", err)
	}
	live := make([]watcherclient.SessionEntry, 0, len(sessions))
	for _, s := range sessions {
		if s.State.SessionID != "" {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return errors.New("no active sessions in watcher state")
	}

	var target watcherclient.SessionEntry
	if len(live) == 1 {
		// Single-session shortcut: skip the router, just expand against the
		// only target — same shortcut saturday-mayor's handle() takes.
		target = live[0]
	} else {
		cands := make([]llm.State, len(live))
		for i, s := range live {
			cands[i] = s.State
		}
		rt, err := llm.RunRoute(b.apiKey, b.cacheDir, text, cands)
		if err != nil {
			return fmt.Errorf("router: %w", err)
		}
		idx, ok := llm.Int(rt, "target_index")
		if !ok || idx < 0 || idx >= len(live) {
			return fmt.Errorf("router returned bad target_index: %v", rt["target_index"])
		}
		conf := llm.Float(rt, "confidence")
		fmt.Fprintf(os.Stderr, "→ route: %s (conf=%.2f) — %s\n",
			live[idx].State.Project, conf, llm.Str(rt, "rationale"))
		if belowThreshold(conf, b.confThreshold) {
			fmt.Fprintf(os.Stderr, "  ↳ router conf below threshold %.2f; skipping\n", b.confThreshold)
			return nil
		}
		target = live[idx]
	}

	exp, err := llm.RunExpand(b.apiKey, b.cacheDir, text, target.State)
	if err != nil {
		return fmt.Errorf("expander: %w", err)
	}
	action := llm.Str(exp, "action")
	expText := llm.Str(exp, "text")
	conf := llm.Float(exp, "confidence")
	switch action {
	case "inject":
		fmt.Fprintf(os.Stderr, "→ %s (conf=%.2f): %s\n", target.State.Project, conf, expText)
		if belowThreshold(conf, b.confThreshold) {
			fmt.Fprintf(os.Stderr, "  ↳ expander conf below threshold %.2f; skipping\n", b.confThreshold)
			return nil
		}
		return b.commitInject(target, inject.WithCallsignRule(expText))
	case "ask":
		// No channel to speak this back yet — Phase 2's job. Log and drop.
		fmt.Fprintf(os.Stderr, "? expander asks (%s), dropped (no reply channel yet): %s\n", target.State.Project, expText)
		return nil
	case "decline":
		fmt.Fprintf(os.Stderr, "✗ expander declined (%s): %s\n", target.State.Project, llm.Str(exp, "rationale"))
		return nil
	default:
		return fmt.Errorf("expander returned unknown action: %q", action)
	}
}

// commitInject mirrors saturday-mayor's commitInject path selection
// (tmux → direct-write → headless) using Phase 0's inject/settle packages
// directly. No audio-sidecar/state-broadcast/blink wiring — mayor-specific
// concerns the backend has no use for. Stage wiring (focus/restore) IS
// shared with mayor now, via stageclient — see the tmux branch below.
func (b *backend) commitInject(target watcherclient.SessionEntry, text string) error {
	if b.dryRun {
		fmt.Fprintln(os.Stderr, "  [dry-run; skipping exec]")
		return nil
	}
	waited, timedOut := settle.WaitForQuiet(target.JSONLPath, b.collisionWait, b.collisionMax)
	if waited > 0 {
		tag := "stable"
		if timedOut {
			tag = "timed-out"
		}
		fmt.Fprintf(os.Stderr, "  ↳ collision-window %s after %s\n", tag, waited.Round(time.Millisecond))
	}
	// Path selection (preferred → fallback), same order as saturday-mayor's
	// commitInject: tmux pane → direct-write → headless.
	if paneID := inject.FindTmuxPane(target.State.Cwd); paneID != "" {
		fmt.Fprintf(os.Stderr, "  ↳ found tmux pane %s for cwd=%s; using tmux send-keys\n", paneID, target.State.Cwd)
		// Captured before the send so restoreWhenSettled's post-inject scan
		// starts from here, not the file's start — otherwise it could match
		// an older assistant block with similar text and restore too early.
		sizeAtInject, err := settle.FileSize(target.JSONLPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ↳ pre-inject size read failed: %v; restore-poll will scan from file start\n", err)
		}
		if err := inject.ViaTmux(paneID, text); err != nil {
			return fmt.Errorf("tmux send-keys: %w", err)
		}
		fmt.Fprintln(os.Stderr, "  ↳ injected via tmux send-keys (live pane handles)")
		b.stage.Write(map[string]any{
			"type":       "focus",
			"session_id": target.State.SessionID,
			"project":    target.State.Project,
			"pane_id":    paneID,
			"cwd":        target.State.Cwd,
			"zoom":       b.stageZoom,
			"tile":       b.stageTile,
		})
		go b.restoreWhenSettled(target, text, sizeAtInject)
		return nil
	}
	fmt.Fprintln(os.Stderr, "  ↳ no tmux pane found for target cwd; using JSONL fallback path")
	if b.injectDirectTokens > 0 && target.JSONLPath != "" {
		est, err := inject.TokensSinceLastCompact(target.JSONLPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ↳ token-estimate failed: %v; falling back to headless inject\n", err)
		} else if est > b.injectDirectTokens {
			fmt.Fprintf(os.Stderr, "  ↳ post-compact tokens-est %d > %d threshold; direct-writing user turn\n",
				est, b.injectDirectTokens)
			if err := inject.DirectWriteUserTurn(target.JSONLPath, target.State.SessionID, target.State.Cwd, text); err != nil {
				return fmt.Errorf("direct-write: %w", err)
			}
			fmt.Fprintf(os.Stderr, "  ↳ direct-wrote user turn to %s\n", target.JSONLPath)
			return nil
		} else {
			fmt.Fprintf(os.Stderr, "  ↳ post-compact tokens-est %d ≤ %d; using headless inject\n",
				est, b.injectDirectTokens)
		}
	}
	n, err := inject.Headless(target.State.SessionID, target.State.Cwd, text)
	if err != nil {
		return fmt.Errorf("claude --resume --print: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  ↳ injected (cwd=%s), %d bytes assistant reply\n", target.State.Cwd, n)
	return nil
}

// restoreWhenSettled polls for the tmux-injected reply to finish, then
// tells stage to restore the pane's normal size. Deliberately minimal —
// no pendingInjects map, no TTL bookkeeping, no spoken narration, none of
// mayor's Phase 3 completion-report filtering. Just enough to know when to
// de-emphasize the pane, reusing settle.AssistantTextAfterInject, the
// primitive Phase 0 extracted from mayor specifically for this reuse.
// Always sends restore on exit, ready or not: saturday-stage's Restore
// handler is a documented no-op for a session it never focused or already
// restored, so it's safe to call unconditionally rather than risk a tile
// stuck expanded after a stuck or crashed session.
func (b *backend) restoreWhenSettled(target watcherclient.SessionEntry, text string, sizeAtInject int64) {
	deadline := time.Now().Add(b.stageRestoreMaxWait)
	for time.Now().Before(deadline) {
		time.Sleep(b.stageRestorePoll)
		_, ready, err := settle.AssistantTextAfterInject(target.JSONLPath, sizeAtInject, text)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ↳ restore-poll read failed for %s: %v\n", target.State.Project, err)
			continue
		}
		if ready {
			break
		}
	}
	b.stage.Write(map[string]any{"type": "restore", "session_id": target.State.SessionID})
}
