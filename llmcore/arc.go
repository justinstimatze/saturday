package llmcore

import (
	"encoding/json"
	"fmt"
)

// V0.2.7 — slow-loop session arc summarizer.
//
// Mayor refreshes one of these per active session every 5 minutes from a
// background goroutine, caches into State.SessionArc, and ships it along to
// the expander. Purpose: arc-awareness — when the user says "deploy that
// thing", the expander has 30 words of context for what "that thing" has
// been across the last hour of work, not just the last assistant turn.
//
// Cheap: Haiku, ~30 tokens out, content-hashed cache. The cost is bounded
// by the number of active sessions × refresh cadence, not by utterance
// volume. With 3-4 active sessions and 5-min cadence, that's ~1 call/min
// total.

const ArcSystem = `You are Saturday's session-arc summarizer.

Given a snapshot of one Claude Code session (project, last user turn, last assistant text, recent tool use, modified files), produce a ≤30-word phrase describing what the session has been WORKING ON across recent turns.

Rules:
- Past-progressive or present-progressive ("debugging X", "wiring Y to Z", "reviewing the …").
- Subject = the work, not the user. Don't say "the user is doing X". Say "X being done".
- Lead with the concrete thing — feature name, file basename, bug — not the verb.
- ≤30 words. No markdown. No preamble. No quoting code.
- If the snapshot is too thin to describe (no last_user_turn AND no last_assistant_text), return an empty string.

Examples:
  state: project=lucida, last_user_turn="run the tests", last_assistant_text="Ran pytest. 32 passed, 2 failed in test_render.py."
  → "lucida — running pytest; isolating two failures in test_render."

  state: project=saturday, last_user_turn="why is the corner tag flickering?", last_assistant_text="Found it — the rewrite goroutine fires while the clear is in flight."
  → "saturday corner-tag flicker bug — race between rewrite goroutine and clear."

  state: project=lucida, modified_files=["render.py","test_render.py"], last_assistant_text="Refactored RenderContext to thread project root through."
  → "lucida render refactor — threading project root through RenderContext."`

var ArcTool = Tool{
	Name:        "arc",
	Description: "Produce the rolling session arc summary.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary": map[string]any{"type": "string", "description": "≤30-word phrase describing what the session has been working on. Empty if snapshot is too thin."},
		},
		"required": []string{"summary"},
	},
}

// RunArc calls the slow-loop summarizer. Returns the rolling arc string, or
// "" if the snapshot is too thin to describe.
//
// Cache-key contract: cid = CacheKey("arc-v1", json.Marshal(state)). State
// is marshaled WITHOUT its own SessionArc field (we don't recurse — caller
// must zero SessionArc before passing or the prior value will be folded in
// to the cache key and we'll miss our own entries).
func RunArc(apiKey, cacheDir string, state State) (string, error) {
	state.SessionArc = "" // never feed prior arc back into its own cache key
	if state.LastUserTurn == "" && state.LastAssistantText == "" {
		return "", nil
	}
	stateJSON, _ := json.MarshalIndent(state, "", "  ")
	userText := fmt.Sprintf("session snapshot:\n%s", string(stateJSON))
	stateKey, _ := json.Marshal(state)
	cid := CacheKey("arc-v1", string(stateKey))
	out, err := CachedCall(apiKey, SummarizerModel, ArcSystem, userText, ArcTool, cacheDir, cid)
	if err != nil {
		return "", err
	}
	if t, ok := out["summary"].(string); ok {
		return t, nil
	}
	return "", fmt.Errorf("arc returned no summary field")
}
