package main

import (
	"math"
	"testing"
)

func TestJaccardSim(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"", "", 0},
		{"hello world", "", 0},
		{"hello world", "hello world", 1},
		{"hello world", "Hello, World!", 1},      // case + punct normalized
		{"hello world", "hello mars", 1.0 / 3.0}, // 1 shared / 3 union
		{"alpha bravo charlie", "charlie delta", 1.0 / 4.0},
		{"a b c", "x y z", 0}, // single chars are dropped → both sets empty
		{"a ab", "ab cd", 1.0 / 2.0},
	}
	for _, c := range cases {
		got := jaccardSim(c.a, c.b)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("jaccardSim(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
