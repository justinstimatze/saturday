package main

import (
	"math"
	"testing"
)

func TestBytesToFloat32RoundTrip(t *testing.T) {
	pcm := []float64{0.5, -0.25, 0.0, 1.0, -1.0}
	b := float64PCMToBytes(pcm)
	got := bytesToFloat32(b)
	if len(got) != len(pcm) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(pcm))
	}
	for i, v := range pcm {
		if math.Abs(float64(got[i])-v) > 1e-6 {
			t.Errorf("sample %d = %v, want %v", i, got[i], v)
		}
	}
}

func TestBytesToFloat32DropsPartialTrailingSample(t *testing.T) {
	// 4 bytes = 1 full sample, plus 2 stray bytes that don't make a
	// second sample — should decode just the one complete sample.
	b := make([]byte, 6)
	got := bytesToFloat32(b)
	if len(got) != 1 {
		t.Errorf("len(got) = %d, want 1", len(got))
	}
}
