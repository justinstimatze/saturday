package main

// V0.3 — expansion-feedback loop (data collection only, V1).
//
// When the saturday-hook helper forwards a UserPromptSubmit event from a
// CC session, mayor scans recent injects sent to that same session. If the
// user-typed prompt has high token-overlap with a recent inject, it's a
// likely "retype" — the user re-typed essentially what we just injected,
// suggesting the inject got swallowed, was wrong, or arrived too late.
//
// V1 ships measurement only — no auto-tuning of thresholds. Retypes are
// logged to ~/.local/state/saturday/feedback.jsonl (one JSON record per
// line) and surfaced visibly in mayor's stderr in the magenta register.
// Later passes can use the corpus to tune --ask-conf, --conf-threshold,
// or per-pattern expander rules.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// recentInjectsMaxAge bounds how far back we look when correlating a
	// prompt_submit against past injects. 10 min covers the realistic
	// retype window — beyond this, a typed prompt is more likely a fresh
	// turn than feedback on a stale inject.
	recentInjectsMaxAge = 10 * time.Minute
	// recentInjectsCap bounds memory; soft cap, the per-session window
	// is the real correctness boundary.
	recentInjectsCap = 200
	// retypeSimThreshold is the Jaccard similarity above which a typed
	// prompt is classified as a retype of a recent inject. 0.30 is
	// deliberately loose — injects often get user-edited slightly before
	// retype (whitespace, polish), and false-positive retype-flagging
	// only pollutes the analysis log, never blocks behavior.
	retypeSimThreshold = 0.30
)

// recentInjectRec is one entry in mayor's recent-inject ring buffer.
// Mayor.recentInjects + Mayor.recentInjectsMu (declared in main.go) are
// the live ring; this file owns the read/write methods.
type recentInjectRec struct {
	SessionID string    `json:"session_id"`
	Project   string    `json:"project"`
	Text      string    `json:"text"`
	TS        time.Time `json:"ts"`
}

// recordRecentInject appends a successful inject to the recent ring,
// pruning entries older than recentInjectsMaxAge and capping count.
// Called from trackInject after a tmux or direct-write inject lands.
func (m *Mayor) recordRecentInject(sessionID, project, text string) {
	if sessionID == "" {
		return
	}
	now := time.Now()
	m.recentInjectsMu.Lock()
	defer m.recentInjectsMu.Unlock()
	m.recentInjects = append(m.recentInjects, recentInjectRec{
		SessionID: sessionID,
		Project:   project,
		Text:      text,
		TS:        now,
	})
	cutoff := now.Add(-recentInjectsMaxAge)
	keep := m.recentInjects[:0]
	for _, r := range m.recentInjects {
		if r.TS.After(cutoff) {
			keep = append(keep, r)
		}
	}
	if len(keep) > recentInjectsCap {
		keep = keep[len(keep)-recentInjectsCap:]
	}
	m.recentInjects = keep
}

// checkRetype scans recent injects for the best match against a typed
// prompt in the same session. Returns the matched record, its Jaccard
// similarity, and whether the similarity exceeds retypeSimThreshold.
func (m *Mayor) checkRetype(sessionID, prompt string) (recentInjectRec, float64, bool) {
	if sessionID == "" || prompt == "" {
		return recentInjectRec{}, 0, false
	}
	m.recentInjectsMu.Lock()
	defer m.recentInjectsMu.Unlock()
	cutoff := time.Now().Add(-recentInjectsMaxAge)
	var best recentInjectRec
	bestSim := 0.0
	for _, r := range m.recentInjects {
		if r.SessionID != sessionID || r.TS.Before(cutoff) {
			continue
		}
		sim := jaccardSim(r.Text, prompt)
		if sim > bestSim {
			best = r
			bestSim = sim
		}
	}
	return best, bestSim, bestSim >= retypeSimThreshold
}

// appendFeedbackRec writes one feedback record (retype or accept) to the
// rolling JSONL. Best-effort — any I/O error is logged once and dropped.
// Path: ~/.local/state/saturday/feedback.jsonl. XDG_STATE_HOME respected
// when set.
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
// token sets. Cheap, no LLM dependency, well-suited to short prompts of
// a few sentences.
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
