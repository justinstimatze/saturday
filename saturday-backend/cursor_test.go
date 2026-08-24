package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCursorLoadMissing(t *testing.T) {
	c := loadCursor(filepath.Join(t.TempDir(), "nope.json"))
	if !c.LastProcessed.IsZero() || len(c.RecentIDs) != 0 {
		t.Errorf("expected zero cursor for a missing file, got %+v", c)
	}
}

func TestCursorSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cursor.json")
	want := cursor{
		LastProcessed: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		RecentIDs:     []string{"a", "b", "c"},
	}
	saveCursor(path, want)
	got := loadCursor(path)
	if !got.LastProcessed.Equal(want.LastProcessed) {
		t.Errorf("LastProcessed = %v, want %v", got.LastProcessed, want.LastProcessed)
	}
	if len(got.RecentIDs) != 3 || got.RecentIDs[0] != "a" || got.RecentIDs[2] != "c" {
		t.Errorf("RecentIDs = %v, want [a b c]", got.RecentIDs)
	}
}

func TestCursorSeen(t *testing.T) {
	c := cursor{RecentIDs: []string{"x", "y"}}
	if !c.seen("x") {
		t.Error("expected x to be seen")
	}
	if c.seen("z") {
		t.Error("expected z not to be seen")
	}
}

func TestCursorAdvance(t *testing.T) {
	base := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	c := cursor{LastProcessed: base}

	c = c.advance("id1", base.Add(time.Minute))
	if !c.LastProcessed.Equal(base.Add(time.Minute)) {
		t.Errorf("LastProcessed didn't advance: got %v", c.LastProcessed)
	}
	if !c.seen("id1") {
		t.Error("id1 should be marked seen after advance")
	}

	// An out-of-order response (earlier createdTime) must not move the
	// cursor backward.
	c = c.advance("id2", base)
	if !c.LastProcessed.Equal(base.Add(time.Minute)) {
		t.Errorf("LastProcessed moved backward: got %v, want unchanged at %v", c.LastProcessed, base.Add(time.Minute))
	}
}

func TestCursorAdvanceCapsRing(t *testing.T) {
	c := cursor{}
	base := time.Now()
	for i := 0; i < recentIDsCap+10; i++ {
		c = c.advance(string(rune('a'+i%26)), base.Add(time.Duration(i)*time.Second))
	}
	if len(c.RecentIDs) != recentIDsCap {
		t.Errorf("RecentIDs len = %d, want %d", len(c.RecentIDs), recentIDsCap)
	}
}
