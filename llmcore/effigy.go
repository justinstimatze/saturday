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
// scoped.
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
	return strings.TrimSpace(b.String())
}
