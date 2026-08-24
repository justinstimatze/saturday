package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// recentIDsCap bounds the dedup ring against exact-timestamp boundary
// duplicates (two notes created in the same second landing on either side
// of a poll). Drive-note volume is low (personal use, not a queue), so this
// is generous headroom, not a tuned limit.
const recentIDsCap = 50

// cursor is saturday-backend's "what have I already processed" state,
// mirroring sync/main.go's cursor pattern: a small JSON file, not a
// database. LastProcessed drives the Drive query (createdTime > cursor);
// RecentIDs is a cheap belt-and-suspenders guard against reprocessing a
// note whose createdTime lands exactly on the cursor boundary.
type cursor struct {
	LastProcessed time.Time `json:"last_processed"`
	RecentIDs     []string  `json:"recent_ids,omitempty"`
}

// loadCursor reads path, returning the zero cursor if the file doesn't
// exist yet or fails to parse — a fresh or corrupt cursor just means every
// note currently in the folder gets (re)processed once.
func loadCursor(path string) cursor {
	data, err := os.ReadFile(path)
	if err != nil {
		return cursor{}
	}
	var c cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return cursor{}
	}
	return c
}

// saveCursor writes c to path atomically (write-tmp, rename) so a crash
// mid-write can't corrupt the cursor file. Fails quiet — a save failure
// just means the next run reprocesses a bit more, never less safe than
// that.
func saveCursor(path string, c cursor) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// seen reports whether id is in the recent-IDs ring.
func (c cursor) seen(id string) bool {
	for _, r := range c.RecentIDs {
		if r == id {
			return true
		}
	}
	return false
}

// advance marks id processed at createdTime, bumping LastProcessed forward
// (never backward — Drive's list is queried in ascending createdTime order,
// but this guards against an out-of-order response) and capping the ring.
func (c cursor) advance(id string, createdTime time.Time) cursor {
	if createdTime.After(c.LastProcessed) {
		c.LastProcessed = createdTime
	}
	c.RecentIDs = append(c.RecentIDs, id)
	if len(c.RecentIDs) > recentIDsCap {
		c.RecentIDs = c.RecentIDs[len(c.RecentIDs)-recentIDsCap:]
	}
	return c
}
