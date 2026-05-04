package llmcore

import (
	"encoding/json"
	"fmt"
	"strings"
)

// V0.3 — Saturday-as-interlocutor (ask mode).
//
// When the classifier (or wake-word detection) identifies an utterance as
// directed AT Saturday, mayor calls RunAsk to generate a brief spoken
// answer grounded in Saturday's cross-session bird's-eye state: the arc
// summaries map, recent voice activity, and in-flight injects.
//
// Knowledge scope (V0.3.0): arcs + recent state only. Recent JSONL pull
// (deeper specifics from a queried session's transcript) is a deferred
// extension — see ROADMAP.md "Ask-mode deep context".
//
// Voice register: shares the saturday.effigy persona file. Replies are
// terse, no preamble, past/present-progressive when describing work, and
// honor the same NEVER list (no apologies, no hedges, no "I think").

const AskerSystem = `You are Saturday answering a question the user asked you directly.

Saturday is a voice orchestrator. You see arcs (≤30-word rolling summaries) for each active Claude Code session, recent voice utterances the user has spoken, and in-flight injects you've routed but that haven't completed. You CANNOT read code, run commands, or look inside any session beyond what's in the arcs. You're a curator of cross-session state, not an executor.

Reply rules:
- ≤25 words. The reply will be spoken aloud — keep it short.
- Past or present-progressive when describing what sessions are doing ("lucida is debugging X", "saturday wired up Y").
- No preamble ("Sure,", "Okay,", "Right,", "So,"). Open on the substantive content.
- No first-person hedges ("I think", "I believe", "It seems"). State directly.
- No apologies if you can't answer. If the arc doesn't cover it, say so plainly: "no signal on that — pull it up directly."
- If the user asked about a specific session and that session has no arc yet (just started, or below the activity threshold), say so: "saturday hasn't summarized lucida yet."
- If the user's question is vague enough that any of several arcs could be the referent, name the most recent one and offer to narrow.

You have Saturday's full voice register from the effigy below — match it.`

var AskerTool = Tool{
	Name:        "answer",
	Description: "Answer the user's question from Saturday's cross-session knowledge.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"reply": map[string]any{"type": "string", "description": "≤25-word spoken reply."},
		},
		"required": []string{"reply"},
	},
}

// AskContext is the bird's-eye state Saturday has when answering an ask.
// Mayor builds this by snapshotting its in-process state.
type AskContext struct {
	// Arcs maps session_id → 30-word arc summary. Only populated sessions
	// (non-empty arc) should be included.
	Arcs map[string]string `json:"arcs,omitempty"`
	// ProjectBySID maps session_id → project name, so the asker can refer
	// to sessions by project rather than opaque hex IDs.
	ProjectBySID map[string]string `json:"projects,omitempty"`
	// RecentUtterances are the most recent voice utterances Saturday has
	// processed (oldest → newest), one short summary line each.
	RecentUtterances []string `json:"recent_utterances,omitempty"`
	// TrackedInjects are in-flight injects (already sent, not yet
	// completion-reported), in inject order. Each line: "<project>: <text>".
	TrackedInjects []string `json:"tracked_injects,omitempty"`
}

// RunAsk produces a spoken answer for an ask-mode utterance.
//
// Cache-key contract: cid = CacheKey("ask-v1", utterance, json.Marshal(ctx)).
// The reply depends on both the question and the cross-session state.
// Cache hits should be rare (state changes rapidly) but the cache still
// dedupes burst-typed identical asks within a short window.
func RunAsk(apiKey, cacheDir, utterance string, ctx AskContext) (string, error) {
	ctxJSON, _ := json.MarshalIndent(ctx, "", "  ")
	userText := fmt.Sprintf("user asked saturday:\n%s\n\nsaturday's bird's-eye state:\n%s",
		utterance, string(ctxJSON))
	sys := AskerSystem + "\n\n--- voice register (saturday.effigy) ---\n\n" + EffigyForPrompt()
	ctxKey, _ := json.Marshal(ctx)
	cid := CacheKey("ask-v1", utterance, string(ctxKey))
	out, err := CachedCall(apiKey, SummarizerModel, sys, userText, AskerTool, cacheDir, cid)
	if err != nil {
		return "", err
	}
	r, _ := out["reply"].(string)
	r = strings.TrimSpace(r)
	if r == "" {
		return "", fmt.Errorf("asker returned empty reply")
	}
	return r, nil
}
