package llmcore

import (
	_ "embed"
	"strings"
)

//go:embed saturday.effigy
var saturdayEffigy string

// EffigyForPrompt returns Saturday's effigy formatted for LLM system-prompt
// inclusion. Strips @-prefixed metadata (@id, @name, @role, etc.) and
// #-prefixed comments; keeps the VOICE / TRAITS / NEVER / QUIRKS blocks
// intact. Appended (not prepended) to expander and summarizer system prompts
// so the persona shapes phrasing of output the task prompt has already
// scoped. A trailing VOCABULARY block is appended when basanite's curated
// known-tics.txt is present — see LoadVocabularyTics.
func EffigyForPrompt() string {
	var b strings.Builder
	for _, line := range strings.Split(saturdayEffigy, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "@") {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	base := strings.TrimSpace(b.String())

	phrases, words := LoadVocabularyTics()
	if len(phrases) == 0 && len(words) == 0 {
		return base
	}
	var v strings.Builder
	v.WriteString(base)
	v.WriteString("\n\nVOCABULARY[\n")
	if len(words) > 0 {
		v.WriteString("  Reach for a different word instead of: " + strings.Join(words, ", ") + ".\n")
	}
	if len(phrases) > 0 {
		v.WriteString("  Never say: \"" + strings.Join(phrases, `", "`) + "\".\n")
	}
	v.WriteString("]")
	return v.String()
}
