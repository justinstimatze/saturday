package llmcore

import (
	"encoding/json"
	"fmt"
	"strings"
)

const RouterSystem = `You are Saturday's router.

The user spoke a brief utterance. Several Claude Code sessions are active across different projects. You see the recent attentional state of each candidate session (cwd, last user turn, last assistant text, last tool use, last tool result tail, modified files), and where available a ` + "`session_arc`" + ` field — a ≤30-word rolling summary of what the session has been working on.

Your job: pick the index of the candidate session the utterance most likely refers to.

The utterance is voice — terse, often anaphoric ("that", "the failing one", "rerun it", "the same one"). Lean on concrete signals: shared filenames, error messages, recent commands, modified files, and ` + "`session_arc`" + ` for the broader theme of each session. The arc is especially load-bearing for anaphoric references when the last-N-turns alone don't disambiguate. If multiple candidates plausibly match, pick the one with the strongest evidence and lower your confidence.

Indices are 0-based.`

var RouterTool = Tool{
	Name:        "route",
	Description: "Pick which candidate session the utterance refers to.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target_index": map[string]any{"type": "integer", "minimum": 0},
			"confidence":   map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"rationale":    map[string]any{"type": "string"},
		},
		"required": []string{"target_index", "confidence", "rationale"},
	},
}

// FormatCandidates renders candidate states for the router prompt.
// Strips Project and SessionID so the model relies on actual signals
// (filenames, recent commands, errors) instead of label matching.
func FormatCandidates(cands []State) string {
	var sb strings.Builder
	for i, c := range cands {
		stripped := c
		stripped.Project = ""
		stripped.SessionID = ""
		js, _ := json.MarshalIndent(stripped, "", "  ")
		fmt.Fprintf(&sb, "candidate %d:\n%s\n\n", i, string(js))
	}
	return sb.String()
}

// RunRoute calls the router LLM with the given utterance against the
// candidate session states. Returns the parsed tool-use input map
// containing target_index, confidence, rationale.
//
// Cache-key contract: cid is derived from ("route-baseline", utterance,
// json.Marshal(cands)). The full cands (with Project/SessionID intact) is
// keyed even though FormatCandidates strips those fields from the prompt
// — this is preserved from pre-lift code, see commit history if surprised.
func RunRoute(apiKey, cacheDir, utterance string, cands []State) (map[string]any, error) {
	candText := FormatCandidates(cands)
	userText := fmt.Sprintf("utterance:\n%s\n\n%s", utterance, candText)
	candKey, _ := json.Marshal(cands)
	// V0.2.8: bumped prefix because RouterSystem now describes session_arc
	// as a routing signal. Old "route-baseline" entries were generated
	// against an arc-unaware prompt and could mislead replays.
	cid := CacheKey("route-baseline-arc-v1", utterance, string(candKey))
	return CachedCall(apiKey, RouterModel, RouterSystem, userText, RouterTool, cacheDir, cid)
}
