package settle

import (
	"os"
	"path/filepath"
	"testing"
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

func TestAssistantTextAfterInject(t *testing.T) {
	t.Run("ready after matching user message", func(t *testing.T) {
		path := writeJSONL(t,
			`{"type":"user","message":{"role":"user","content":"run the tests"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"tests pass"}]}}`,
		)
		text, ready, err := AssistantTextAfterInject(path, 0, "run the tests")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ready || text != "tests pass" {
			t.Errorf("got (%q, %v), want (\"tests pass\", true)", text, ready)
		}
	})

	t.Run("not ready when chain still running", func(t *testing.T) {
		path := writeJSONL(t,
			`{"type":"user","message":{"role":"user","content":"run the tests"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","text":""}]}}`,
		)
		_, ready, err := AssistantTextAfterInject(path, 0, "run the tests")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ready {
			t.Error("expected ready=false while the last block is tool_use")
		}
	})

	t.Run("not ready when inject text never surfaces as a user message", func(t *testing.T) {
		path := writeJSONL(t,
			`{"type":"user","message":{"role":"user","content":"unrelated turn"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
		)
		_, ready, err := AssistantTextAfterInject(path, 0, "run the tests")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ready {
			t.Error("expected ready=false when injectText never appears in a user message")
		}
	})

	t.Run("ignores assistant text preceding the matching user message", func(t *testing.T) {
		path := writeJSONL(t,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"stale reply from before our inject"}]}}`,
			`{"type":"user","message":{"role":"user","content":"run the tests"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"fresh reply"}]}}`,
		)
		text, ready, err := AssistantTextAfterInject(path, 0, "run the tests")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ready || text != "fresh reply" {
			t.Errorf("got (%q, %v), want (\"fresh reply\", true)", text, ready)
		}
	})
}
