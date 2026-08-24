package inject

import (
	"os"
	"path/filepath"
	"strings"
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

func TestTokensSinceLastCompact(t *testing.T) {
	t.Run("no compact marker counts the whole file", func(t *testing.T) {
		line := `{"type":"user","message":{"content":"hello"}}`
		path := writeJSONL(t, line)
		got, err := TokensSinceLastCompact(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := (len(line) + 1) / 4
		if got != want {
			t.Errorf("got %d, want %d", got, want)
		}
	})

	t.Run("only counts bytes after the last compact marker", func(t *testing.T) {
		pre := `{"type":"user","message":{"content":"pre-compact turn, should not be counted"}}`
		marker := `{"type":"summary","isCompactSummary":true}`
		post := `{"type":"user","message":{"content":"post"}}`
		path := writeJSONL(t, pre, marker, post)
		got, err := TokensSinceLastCompact(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := (len(post) + 1) / 4
		if got != want {
			t.Errorf("got %d, want %d (should only count the post-compact line)", got, want)
		}
	})
}

func TestWithCallsignRule(t *testing.T) {
	got := WithCallsignRule("here are the options")
	if !strings.HasPrefix(got, "here are the options") {
		t.Errorf("expected original text preserved as a prefix, got %q", got)
	}
	if !strings.HasSuffix(got, CallsignRule) {
		t.Error("expected CallsignRule appended as a suffix")
	}
}

func TestTags(t *testing.T) {
	if got := ActiveTag("lucida"); !strings.Contains(got, "lucida") {
		t.Errorf("ActiveTag(%q) = %q, want it to contain the project name", "lucida", got)
	}
	if got := DoneTag("lucida"); !strings.Contains(got, "lucida") {
		t.Errorf("DoneTag(%q) = %q, want it to contain the project name", "lucida", got)
	}
	if ActiveTag("x") == DoneTag("x") {
		t.Error("ActiveTag and DoneTag should render differently")
	}
}
