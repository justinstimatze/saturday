package main

import (
	"testing"
	"time"
)

func TestNextBackoffDoublesAndCaps(t *testing.T) {
	max := 16 * time.Second
	cur := time.Second
	for i := 0; i < 3; i++ {
		cur = nextBackoff(cur, max)
	}
	if cur != 8*time.Second {
		t.Errorf("after 3 doublings from 1s, got %v, want 8s", cur)
	}
	cur = nextBackoff(cur, max)
	if cur != max {
		t.Errorf("doubling past max should cap at max: got %v, want %v", cur, max)
	}
	cur = nextBackoff(cur, max)
	if cur != max {
		t.Errorf("staying at max should not exceed it: got %v, want %v", cur, max)
	}
}
