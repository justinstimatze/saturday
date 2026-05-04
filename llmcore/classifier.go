package llmcore

import (
	"fmt"
)

// V0.3 — utterance classifier. Distinguishes utterances directed AT Saturday
// (asking for an answer from Saturday's own knowledge) from utterances VIA
// Saturday (a request to be relayed/injected into a CC session).
//
// Bias: when ambiguous, return inject. False-positive ask (real CC request
// silently swallowed and answered by Saturday from arc-only context) is
// worse than false-negative ask (user retypes with a wake-word). Mayor
// applies a confidence threshold on top of this — only confidence > thresh
// AND type == "ask" routes to the ask path.

const ClassifierSystem = `You are Saturday's utterance classifier.

Saturday is a voice orchestrator that does two distinct things:
1. ROUTE / INJECT — relay a user's voice request into one of several active Claude Code sessions, where another agent (the CC session) will action it.
2. ANSWER (ask-mode) — answer the user directly from Saturday's own bird's-eye knowledge of what's going on across all sessions (rolling arc summaries, recent voice activity, in-flight injects). Saturday has NO power to read or run code, only to summarize cross-session state.

Your job: given a single voice utterance, decide which mode it belongs to.

Heuristics for ASK:
- Direct second-person address to Saturday ("hey saturday", "saturday,").
- Questions about Saturday's own state or cross-session view ("what's on", "what am I doing", "summarize lucida", "anything blocked", "what was that just now").
- Status-of-the-orchestrator questions, not status-of-a-codebase questions.

Heuristics for INJECT:
- Imperatives directed at code: "fix", "run", "deploy", "rerun", "investigate", "show me the diff in X", "what's in file Y".
- Concrete-task language: filenames, function names, error messages, numbers.
- Anaphoric references to a session's own work ("the failing one", "rerun it", "the bravo callsign").

Bias toward INJECT when ambiguous — utterances that *could* be answered by either Saturday's arc or by a CC session should default to inject (the CC will answer if it can; Saturday's arc is a coarser fallback). Reserve ASK for utterances that could *only* meaningfully be answered by Saturday's cross-session view.

Examples:
  "what's on" → ask, conf 0.95
  "saturday what was I doing in lucida" → ask, conf 0.95
  "rerun the lucida tests" → inject, conf 0.95
  "fix the bear callsign" → inject, conf 0.9
  "what was that error" → inject, conf 0.6 (could be either, default inject)
  "summarize what's happening across sessions" → ask, conf 0.85
  "what's the status of the lucida render bug" → inject, conf 0.55 (CC can answer better than arc; default inject)`

var ClassifierTool = Tool{
	Name:        "classify",
	Description: "Classify the utterance as ask (to Saturday) or inject (via Saturday).",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"type":       map[string]any{"type": "string", "enum": []string{"ask", "inject"}},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"rationale":  map[string]any{"type": "string"},
		},
		"required": []string{"type", "confidence", "rationale"},
	},
}

// RunClassify classifies an utterance. Returns ("ask"|"inject", confidence,
// rationale, error). On any error, callers should treat as inject — the
// classifier is a UX optimization, not a load-bearing safety check.
//
// Cache-key contract: cid = CacheKey("classify-v1", utterance). The
// classification is purely a function of the utterance text, not of session
// state.
func RunClassify(apiKey, cacheDir, utterance string) (string, float64, string, error) {
	userText := fmt.Sprintf("utterance:\n%s", utterance)
	cid := CacheKey("classify-v1", utterance)
	out, err := CachedCall(apiKey, RouterModel, ClassifierSystem, userText, ClassifierTool, cacheDir, cid)
	if err != nil {
		return "", 0, "", err
	}
	t, _ := out["type"].(string)
	conf, _ := out["confidence"].(float64)
	rat, _ := out["rationale"].(string)
	if t != "ask" && t != "inject" {
		return "", 0, "", fmt.Errorf("classifier returned bad type: %v", out["type"])
	}
	return t, conf, rat, nil
}
