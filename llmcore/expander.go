package llmcore

import (
	"encoding/json"
	"fmt"
)

const ExpanderSystem = `You are Saturday's expander.

Given a brief user utterance (a transcribed voice command, prose-style) and a JSON snapshot of the surrounding Claude Code session's recent state, produce the prompt to inject into that session.

Rules:
- action="inject" — DEFAULT. Expand the utterance into a clean instruction the live session will act on. When the utterance is somewhat ambiguous, pick the most likely interpretation, log the assumption in rationale, and proceed. Always populate ` + "`confirmation`" + ` with a ≤8-word spoken narration in the user's terms.
- action="ask" — RESERVE for genuinely high-stakes ambiguity ONLY: destructive or irreversible operations (rm, git push --force, drop table, mass-delete, irrevocable network actions) where guessing wrong causes data loss or harm. NOT for normal disambiguation — those default to inject with a best-guess.
- action="decline" — utterance is not actionable in this session, OR low-stakes-but-too-ambiguous to pick a reasonable default. Better to silently skip than to nag with a clarifier.

The Jarvis pattern: act first with best-guess, narrate briefly. Don't quiz the user with two-option questions for ambient ambiguity — that breaks flow.

CRITICAL: Do NOT answer the user's question yourself, even when the state would let you. Your job is to produce an instruction the live session executes. Use state to GROUND the expansion (which session, which file, which subject) — never to PRE-RESOLVE the answer.

Example (no pre-resolve):
  utterance: "list the changed files"
  state.modified_files: ["a.py", "b.py", "c.py"]
  WRONG: text="List the changed files: a.py, b.py, c.py."
  RIGHT: text="List the changed files.", confirmation="listing changed files"

Example (default-instead-of-ask):
  utterance: "rerun the failing one"
  state shows one failing test, two passing
  WRONG action="ask", text="Which test do you want to rerun?"
  RIGHT action="inject", text="Re-run the test that failed in the previous run.", confirmation="rerunning failing test", rationale="single failing test in scope; defaulting to that"

Example (ask is correct here):
  utterance: "delete that one"
  state shows several similar files recently mentioned
  RIGHT action="ask", text="Delete which file — a.py, b.py, or c.py?" — destructive + ambiguous

The injected text should be terse and project-specific. Reference concrete things from state ONLY when they disambiguate intent. Do NOT add preamble or explanation. Do NOT exceed 3 sentences. Match the level of detail Claude Code would act on cleanly.

When state contains ` + "`session_arc`" + ` (a ≤30-word rolling summary of what the session has been working on across recent turns), use it as background to disambiguate referents in the utterance — "deploy that thing", "rerun it", "the same one again". Don't quote the arc back at the user; ground silently and proceed.

` + "`confirmation`" + ` (required for action=inject, empty for ask/decline): ≤8-word phrase Saturday will speak aloud as the action starts. Match the user's verbs and terms. Examples: "running git status", "rerunning failing test", "checking changed files in lucida".`

var ExpanderTool = Tool{
	Name:        "expand",
	Description: "Produce the expanded injection prompt.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":       map[string]any{"type": "string", "enum": []string{"inject", "ask", "decline"}},
			"text":         map[string]any{"type": "string"},
			"confidence":   map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"rationale":    map[string]any{"type": "string"},
			"confirmation": map[string]any{"type": "string", "description": "≤8-word spoken narration for TTS when action=inject. Empty for ask/decline."},
		},
		"required": []string{"action", "text", "confidence", "rationale"},
	},
}

// RunExpand calls the expander LLM with the utterance and target session
// state. Returns the parsed tool-use input containing action, text,
// confidence, rationale.
//
// Cache-key contract: cid = CacheKey("expand-v3", utterance,
// json.Marshal(target)). The "expand-v3" prefix supersedes "expand-v2" after
// the V0.2.2 Jarvis-pattern prompt update: hard bias against `ask`, default
// to inject-with-best-guess + `confirmation` field for TTS narration. Old
// eval cache entries under "expand-v2" / "expand-baseline" are orphaned —
// re-run eval/expander_backtest to refresh the pass-rate baseline.
//
// V0.2.7 attempt at expand-v4 (added rules for continuation gestures,
// "questions are actionable", "decline when antecedent missing") regressed
// pass rate 77% → 57% — net 4 fixed / 10 regressed. Reverted. Lesson: the
// v3 prompt is at a local optimum on this corpus; the over_cautious bias is
// likely as much in the judge's grading as in the expander's behavior.
// Future attempts should swap the expander to Sonnet, refresh the corpus,
// or audit the judge rubric — not just append more rules.
//
// V0.2.7 (continued): "expand-v3-sonnet-effigy" appends the saturday.effigy
// VOICE / NEVER / QUIRKS guidance to the system prompt. Shapes phrasing of
// `confirmation` (≤8-word spoken narration) and `text` (the inject) toward
// the terse / past-tense / no-hedge register without changing the action
// rules. Cache prefix bumped so prior Sonnet caches don't poison voice
// register output.
//
// V0.2.7 (continued): "expand-v3-sonnet-effigy-arc" adds the session_arc
// guidance line — uses arc summary as grounding context for ambiguous
// referents ("that thing", "rerun it") without quoting back. State.SessionArc
// is populated by mayor's runArcRefresher (slow-loop, 5min cadence). Cache
// prefix bumped so prior arc-blind caches don't shadow arc-aware output.
func RunExpand(apiKey, cacheDir, utterance string, target State) (map[string]any, error) {
	stateJSON, _ := json.MarshalIndent(target, "", "  ")
	userText := fmt.Sprintf("utterance:\n%s\n\nsession state:\n%s", utterance, string(stateJSON))
	stateKey, _ := json.Marshal(target)
	sys := ExpanderSystem + "\n\n--- voice register (saturday.effigy) ---\n\n" + EffigyForPrompt()
	cid := CacheKey("expand-v3-sonnet-effigy-arc", utterance, string(stateKey))
	return CachedCall(apiKey, ExpanderModel, sys, userText, ExpanderTool, cacheDir, cid)
}
