package orchestrator

// Expansion-feedback loop (data collection only).
//
// When a caller's own hook listener (e.g. saturday-mayor's saturday-hook
// socket) forwards a UserPromptSubmit event, it calls CheckRetype to scan
// recent injects sent to that same session. If the user-typed prompt has
// high token-overlap with a recent inject, it's a likely "retype" — the
// user re-typed essentially what was just injected, suggesting the inject
// got swallowed, was wrong, or arrived too late. CheckRetype logs the
// match to ~/.local/state/saturday/feedback.jsonl itself; the caller only
// needs the returned record for its own stderr logging.
//
// Ships measurement only — no auto-tuning of thresholds. Later passes can
// use the corpus to tune AskConf, ConfThreshold, or per-pattern expander
// rules.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// recentInjectsMaxAge bounds how far back CheckRetype looks when
	// correlating a prompt against past injects. 10 min covers the
	// realistic retype window — beyond this, a typed prompt is more
	// likely a fresh turn than feedback on a stale inject.
	recentInjectsMaxAge = 10 * time.Minute
	// recentInjectsCap bounds memory; soft cap, the per-session window is
	// the real correctness boundary.
	recentInjectsCap = 200
	// retypeSimThreshold is the Jaccard similarity above which a typed
	// prompt is classified as a retype of a recent inject. 0.30 is
	// deliberately loose — injects often get user-edited slightly before
	// retype (whitespace, polish), and a false-positive flag only
	// pollutes the analysis log, never blocks behavior.
	retypeSimThreshold = 0.30
)

// recordRecentInject appends a successful inject to the recent ring,
// pruning entries older than recentInjectsMaxAge and capping count.
// Called from trackInject after a tmux or direct-write inject lands.
func (o *Orchestrator) recordRecentInject(sessionID, project, text string) {
	if sessionID == "" {
		return
	}
	now := time.Now()
	o.recentInjectsMu.Lock()
	defer o.recentInjectsMu.Unlock()
	o.recentInjects = append(o.recentInjects, RecentInjectRec{
		SessionID: sessionID,
		Project:   project,
		Text:      text,
		TS:        now,
	})
	cutoff := now.Add(-recentInjectsMaxAge)
	keep := o.recentInjects[:0]
	for _, r := range o.recentInjects {
		if r.TS.After(cutoff) {
			keep = append(keep, r)
		}
	}
	if len(keep) > recentInjectsCap {
		keep = keep[len(keep)-recentInjectsCap:]
	}
	o.recentInjects = keep
}

// CheckRetype scans recent injects for the best match against a typed
// prompt in the same session. Returns the matched record, its Jaccard
// similarity, and whether the similarity exceeds retypeSimThreshold. On a
// match, also appends a feedback record to
// ~/.local/state/saturday/feedback.jsonl (XDG_STATE_HOME respected) —
// best-effort, any I/O error is logged once and dropped.
func (o *Orchestrator) CheckRetype(sessionID, prompt string) (RecentInjectRec, float64, bool) {
	if sessionID == "" || prompt == "" {
		return RecentInjectRec{}, 0, false
	}
	o.recentInjectsMu.Lock()
	cutoff := time.Now().Add(-recentInjectsMaxAge)
	var best RecentInjectRec
	bestSim := 0.0
	for _, r := range o.recentInjects {
		if r.SessionID != sessionID || r.TS.Before(cutoff) {
			continue
		}
		sim := jaccardSim(r.Text, prompt)
		if sim > bestSim {
			best = r
			bestSim = sim
		}
	}
	o.recentInjectsMu.Unlock()

	isRetype := bestSim >= retypeSimThreshold
	if isRetype {
		appendFeedbackRec(map[string]any{
			"ts":                 float64(time.Now().UnixNano()) / 1e9,
			"event":              "retype",
			"session_id":         sessionID,
			"project":            best.Project,
			"inject_text":        best.Text,
			"prompt_text":        prompt,
			"similarity":         bestSim,
			"inject_age_seconds": time.Since(best.TS).Seconds(),
		})
	}
	return best, bestSim, isRetype
}

// appendFeedbackRec writes one feedback record (currently just "retype")
// to the rolling JSONL. Best-effort — any I/O error is logged once and
// dropped. Path: ~/.local/state/saturday/feedback.jsonl. XDG_STATE_HOME
// respected when set.
func appendFeedbackRec(rec map[string]any) {
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		root = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	dir := filepath.Join(root, "saturday")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "  \033[2m↳ feedback log mkdir: %v\033[0m\n", err)
		return
	}
	path := filepath.Join(dir, "feedback.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  \033[2m↳ feedback log open: %v\033[0m\n", err)
		return
	}
	defer f.Close()
	body, _ := json.Marshal(rec)
	body = append(body, '\n')
	_, _ = f.Write(body)
}

// jaccardSim computes token-set Jaccard similarity between two strings.
// Lower-cased, alphanumeric tokenization. 0 = no overlap, 1 = identical
// token sets. Cheap, no LLM dependency, well-suited to short prompts of a
// few sentences.
func jaccardSim(a, b string) float64 {
	tokA := tokenSet(a)
	tokB := tokenSet(b)
	if len(tokA) == 0 || len(tokB) == 0 {
		return 0
	}
	inter := 0
	for t := range tokA {
		if tokB[t] {
			inter++
		}
	}
	union := len(tokA) + len(tokB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			tok := b.String()
			if len(tok) > 1 { // drop single-char noise
				out[tok] = true
			}
			b.Reset()
		}
	}
	if b.Len() > 1 {
		out[b.String()] = true
	}
	return out
}
