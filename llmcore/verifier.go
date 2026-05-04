package llmcore

import (
	"regexp"
	"strings"
)

// Effigy verifier — V0.2.7. Cheap lexical post-pass over summarizer output
// catching the most common NEVER violations (apologies, hedges, preamble,
// exclamation, performed feeling). Repairs by stripping the offending
// phrase rather than re-rolling — keeps latency identical to the
// no-verifier path. Only runs on summarizer output (Phase 3 spoken
// completions); the expander's `confirmation` field is short enough that
// the system prompt alone keeps it in register.
//
// Design choice: lexical patterns over a model-judge re-roll. A re-roll
// is more accurate but doubles the latency for every Phase 3 report and
// costs another LLM round trip. The lexical patterns catch the obvious
// register violations (90% of what the NEVER list cares about) at zero
// extra cost. If the patterns leave the text empty or near-empty after
// stripping, we fall back to "Done." rather than emitting nothing.

// strikePatterns are case-insensitive regex matches that get stripped from
// summarizer output. Each pattern targets one NEVER rule from
// saturday.effigy. Order matters when patterns overlap; longest-first.
var strikePatterns = []*regexp.Regexp{
	// Apologies — Never apologize.
	regexp.MustCompile(`(?i)\b(?:sorry[,!.]?\s*|apologies\s+for\s+\S+\s*|my\s+mistake[,.!]?\s*)`),
	// Hedges — Never hedge.
	regexp.MustCompile(`(?i)\b(?:i\s+think\s+|i\s+believe\s+|i\s+might\s+be\s+wrong[,.]?\s*|maybe\s+|perhaps\s+|if\s+that['']s\s+okay[,.]?\s*|if\s+you['']?d\s+like[,.]?\s*)`),
	// Preamble — Never preamble.
	regexp.MustCompile(`(?i)^(?:so\s+i['']?m\s+going\s+to\s+|so\s+i['']?ll\s+|let\s+me\s+|just\s+to\s+confirm[,.]?\s*|going\s+to\s+|i['']?m\s+going\s+to\s+|i['']?ll\s+)`),
	// Performed feeling — Never claim feeling.
	regexp.MustCompile(`(?i)\b(?:happy\s+to\s+|glad\s+to\s+|excited\s+to\s+|i['']?m\s+excited\s+|i['']?m\s+glad\s+|i['']?m\s+happy\s+)`),
}

// strikeExclamation collapses runs of !!! → . and standalone ! → .
// Never use exclamation marks.
var strikeExclamation = regexp.MustCompile(`!+`)

// VerifyAndRepair runs the effigy NEVER list against text. Returns the
// cleaned string and a count of violations stripped. Empty cleaned output
// degrades to the "Done." fallback at the call site.
func VerifyAndRepair(text string) (cleaned string, violations int) {
	cleaned = text
	for _, p := range strikePatterns {
		matches := p.FindAllStringIndex(cleaned, -1)
		violations += len(matches)
		cleaned = p.ReplaceAllString(cleaned, "")
	}
	if strikeExclamation.MatchString(cleaned) {
		violations++
		cleaned = strikeExclamation.ReplaceAllString(cleaned, ".")
	}
	// Tidy up: collapse multiple spaces, trim, capitalize first letter
	// since we may have stripped a sentence opener.
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned != "" {
		// Capitalize first letter.
		runes := []rune(cleaned)
		if runes[0] >= 'a' && runes[0] <= 'z' {
			runes[0] = runes[0] - 'a' + 'A'
			cleaned = string(runes)
		}
	}
	return cleaned, violations
}
