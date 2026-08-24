package main

import "testing"

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
