package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	llm "saturday/llmcore"
	"saturday/watcherclient"
)

func writeJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	var body string
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestBelowThreshold(t *testing.T) {
	cases := []struct {
		name      string
		conf      float64
		threshold float64
		want      bool
	}{
		{"zero threshold disables the gate", 0.0, 0, false},
		{"exact match at default fails — coin-flip confidence, not a pass", 0.50, 0.50, true},
		{"just above threshold passes", 0.51, 0.50, false},
		{"well below threshold fails", 0.10, 0.50, true},
		{"well above threshold passes", 0.95, 0.50, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := belowThreshold(c.conf, c.threshold); got != c.want {
				t.Errorf("belowThreshold(%.2f, %.2f) = %v, want %v", c.conf, c.threshold, got, c.want)
			}
		})
	}
}

// TestRestoreWhenSettled covers the new stage-restore wiring directly,
// rather than through commitInject's tmux branch — faking a real tmux
// pane with a live "claude" process for FindTmuxPane to discover would be
// its own separate testing effort, out of proportion to this addition.
// A zero-value backend.stage (no --stage-sock set, same as every existing
// test in this file) proves the no-op-when-unconfigured guarantee holds
// on both the fast and the timeout path.
func TestRestoreWhenSettled(t *testing.T) {
	t.Run("returns promptly once the reply settles", func(t *testing.T) {
		path := writeJSONL(t,
			`{"type":"user","message":{"role":"user","content":"run the tests"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"tests pass"}]}}`,
		)
		b := &backend{stageRestorePoll: 10 * time.Millisecond, stageRestoreMaxWait: 2 * time.Second}
		target := watcherclient.SessionEntry{
			State:     llm.State{SessionID: "sess-1", Project: "spar"},
			JSONLPath: path,
		}
		start := time.Now()
		b.restoreWhenSettled(target, "run the tests", 0)
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Errorf("expected an early return once ready, took %s", elapsed)
		}
	})

	t.Run("gives up at stageRestoreMaxWait if the reply never settles", func(t *testing.T) {
		path := writeJSONL(t,
			`{"type":"user","message":{"role":"user","content":"run the tests"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","text":""}]}}`,
		)
		b := &backend{stageRestorePoll: 10 * time.Millisecond, stageRestoreMaxWait: 60 * time.Millisecond}
		target := watcherclient.SessionEntry{
			State:     llm.State{SessionID: "sess-2", Project: "lucida"},
			JSONLPath: path,
		}
		start := time.Now()
		b.restoreWhenSettled(target, "run the tests", 0)
		if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
			t.Errorf("expected to run for roughly stageRestoreMaxWait, returned after %s", elapsed)
		}
	})
}
