package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBuildQuery(t *testing.T) {
	t.Run("zero since omits the createdTime clause", func(t *testing.T) {
		// Regression: a fresh cursor's zero-value time.Time formats as year
		// 0001, which the real Drive API rejected with "Error 400: Invalid
		// Value, invalid" — caught by a live smoke test, not make ci.
		got := buildQuery("folder123", time.Time{})
		want := "'folder123' in parents and trashed = false"
		if got != want {
			t.Errorf("buildQuery with zero since = %q, want %q", got, want)
		}
	})

	t.Run("non-zero since includes the createdTime clause", func(t *testing.T) {
		since := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
		got := buildQuery("folder123", since)
		want := "'folder123' in parents and trashed = false and createdTime > '2026-08-24T12:00:00Z'"
		if got != want {
			t.Errorf("buildQuery with non-zero since = %q, want %q", got, want)
		}
	})
}

func TestIsGoogleDoc(t *testing.T) {
	cases := []struct {
		mimeType string
		want     bool
	}{
		{"application/vnd.google-apps.document", true},
		{"text/plain", false},
		{"text/markdown", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isGoogleDoc(c.mimeType); got != c.want {
			t.Errorf("isGoogleDoc(%q) = %v, want %v", c.mimeType, got, c.want)
		}
	}
}

// fakeDriveSource is a driveSource that returns a fixed note list,
// regardless of `since` — pollOnce's cursor logic is what's under test, not
// server-side filtering.
type fakeDriveSource struct {
	notes []note
}

func (f *fakeDriveSource) ListNew(ctx context.Context, since time.Time) ([]note, error) {
	return f.notes, nil
}

func TestPollOnceProcessesEachNoteOnce(t *testing.T) {
	base := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	src := &fakeDriveSource{notes: []note{
		{ID: "n1", CreatedTime: base, Text: "first"},
		{ID: "n2", CreatedTime: base.Add(time.Minute), Text: "second"},
	}}

	var processed []string
	process := func(text string) error {
		processed = append(processed, text)
		return nil
	}

	c := pollOnce(context.Background(), src, cursor{}, process)

	if len(processed) != 2 || processed[0] != "first" || processed[1] != "second" {
		t.Fatalf("processed = %v, want [first second]", processed)
	}
	if !c.LastProcessed.Equal(base.Add(time.Minute)) {
		t.Errorf("cursor.LastProcessed = %v, want %v", c.LastProcessed, base.Add(time.Minute))
	}
	if !c.seen("n1") || !c.seen("n2") {
		t.Error("expected both notes marked seen")
	}
}

func TestPollOnceSkipsAlreadySeen(t *testing.T) {
	base := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	src := &fakeDriveSource{notes: []note{
		{ID: "n1", CreatedTime: base, Text: "first"},
		{ID: "n2", CreatedTime: base.Add(time.Minute), Text: "second"},
	}}

	var processed []string
	process := func(text string) error {
		processed = append(processed, text)
		return nil
	}

	// n1 already seen from a prior poll — should be skipped this time.
	start := cursor{RecentIDs: []string{"n1"}}
	pollOnce(context.Background(), src, start, process)

	if len(processed) != 1 || processed[0] != "second" {
		t.Fatalf("processed = %v, want [second]", processed)
	}
}

func TestPollOnceMarksSeenEvenOnProcessError(t *testing.T) {
	base := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	src := &fakeDriveSource{notes: []note{
		{ID: "n1", CreatedTime: base, Text: "broken"},
		{ID: "n2", CreatedTime: base.Add(time.Minute), Text: "fine"},
	}}

	var processed []string
	process := func(text string) error {
		processed = append(processed, text)
		if text == "broken" {
			return errors.New("boom")
		}
		return nil
	}

	c := pollOnce(context.Background(), src, cursor{}, process)

	if len(processed) != 2 {
		t.Fatalf("expected both notes to be attempted, got %v", processed)
	}
	if !c.seen("n1") {
		t.Error("a failed note should still be marked seen — no retry loop, matches the rest of this codebase")
	}
}
