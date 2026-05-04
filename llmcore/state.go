package llmcore

// State is the attentional snapshot of one Claude Code session that the
// router and expander consume. Wire-compatible with watcher/main.go's
// State (the watcher publishes this shape over its Unix socket); the
// duplication on the watcher side is deliberate so the watcher binary
// can stay on pure stdlib without depending on llmcore.
//
// Field-add discipline: any new field added here must also be added to
// watcher/main.go's State, in the same JSON-tag order, or marshal output
// will diverge and the cache keys (which use json.Marshal on State)
// will silently drift.
type State struct {
	Project            string       `json:"project"`
	SessionID          string       `json:"session_id,omitempty"`
	Cwd                string       `json:"cwd,omitempty"`
	LastUserTurn       string       `json:"last_user_turn,omitempty"`
	LastAssistantText  string       `json:"last_assistant_text,omitempty"`
	LastToolUse        *ToolUseSnap `json:"last_tool_use,omitempty"`
	LastToolResultTail string       `json:"last_tool_result_tail,omitempty"`
	ModifiedFiles      []string     `json:"modified_files,omitempty"`
	// V0.2.7: rolling ≤30-word arc summary from llmcore.RunArc, refreshed by
	// mayor every 5 min. Watcher itself never populates this — it stays empty
	// in the watcher socket payload — so the omitempty keeps marshal output
	// identical to pre-V0.2.7 when arc is absent (cache key compatibility).
	SessionArc string `json:"session_arc,omitempty"`
}

type ToolUseSnap struct {
	Name         string `json:"name"`
	InputSummary string `json:"input_summary"`
}
