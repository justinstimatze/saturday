package llmcore

import (
	"fmt"
)

// Phase 3 — proactive completion reports.
//
// After a successful inject, the mayor tracks the target session and watches
// the JSONL for completion (latest assistant block is text, JSONL stable for
// N seconds). When that trips, it asks Haiku to produce a ≤15-word past-tense
// completion phrase and speaks it via the audio sidecar. The user gets
// "lucida tests done — 32 passed, 2 failed" without having to ask.

const SummarizerSystem = `You are Saturday's completion-summarizer.

Given the user's original voice command and Claude Code's final assistant text after acting on it, produce a ≤15-word completion report Saturday will SPEAK ALOUD when the task finishes.

Rules:
- Past tense ("finished", "found", "applied").
- Lead with what the user wanted to know — not the process.
- If the result reveals a key fact (count, status, error, file, version), include it.
- ≤15 words. No markdown. No quoting code. No preamble like "Saturday says" or "Done:".
- Match the user's verbs. If they said "tests", say "tests". Don't paraphrase to "test suite".
- If the result is unclear or empty, return a brief generic completion ("Done.") rather than guessing.

Examples:
  utterance: "run the lucida tests"
  result: "Ran pytest. 32 passed, 2 failed in test_render.py."
  → "Lucida tests done — 32 passed, 2 failed in test_render."

  utterance: "what's in the changelog"
  result: "CHANGELOG.md latest entry: V0.2.3 — XDG migration and stack launcher."
  → "Latest changelog entry is V0.2.3 — XDG migration and stack launcher."

  utterance: "deploy to staging"
  result: "Deployed via ./deploy.sh staging. Health check returned 200 OK."
  → "Staging deploy succeeded — health check is 200."

  utterance: "fix the lint errors"
  result: "Applied 3 fixes across 2 files. ruff now clean."
  → "Fixed 3 lint errors across two files; ruff clean."`

var SummarizerTool = Tool{
	Name:        "summarize",
	Description: "Produce the spoken completion report.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "≤15-word past-tense completion report."},
		},
		"required": []string{"text"},
	},
}

// RunSummarize calls the summarizer LLM with the original utterance and the
// post-inject final assistant text. Returns the spoken completion string.
//
// Cache-key contract: cid = CacheKey("summary-v2", utterance, result). The
// "summary-v2" prefix supersedes "summary-v1" after V0.2.7 wiring of the
// saturday.effigy voice register into the system prompt. Bump on prompt
// change so old entries don't poison new behavior.
func RunSummarize(apiKey, cacheDir, utterance, result string) (string, error) {
	userText := fmt.Sprintf("utterance:\n%s\n\nresult:\n%s", utterance, result)
	sys := SummarizerSystem + "\n\n--- voice register (saturday.effigy) ---\n\n" + EffigyForPrompt()
	cid := CacheKey("summary-v2", utterance, result)
	out, err := CachedCall(apiKey, SummarizerModel, sys, userText, SummarizerTool, cacheDir, cid)
	if err != nil {
		return "", err
	}
	if t, ok := out["text"].(string); ok {
		// V0.2.7: effigy verifier strips NEVER-list violations (apologies,
		// hedges, preamble, exclamation, performed feeling). Cheap lexical
		// pass — no extra LLM call. If the strip leaves nothing useful,
		// fall back to "Done." rather than emitting empty.
		cleaned, violations := VerifyAndRepair(t)
		if violations > 0 && cleaned != t {
			// Trace the strip so we can audit register drift over time.
			// Empty cleaned → degrade to "Done."; non-empty → use cleaned.
			if cleaned == "" {
				cleaned = "Done."
			}
		}
		return cleaned, nil
	}
	return "", fmt.Errorf("summarizer returned no text field")
}
