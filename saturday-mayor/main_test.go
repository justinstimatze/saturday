package main

import "testing"

func TestParseActivity(t *testing.T) {
	cases := []struct {
		in         string
		wantState  string
		wantTarget string
	}{
		{"", "idle", ""},
		{"idle", "idle", ""},
		{"routing", "routing", ""},
		{"injecting → lucida", "injecting", "lucida"},
		{"injecting → some-project-with-dashes", "injecting", "some-project-with-dashes"},
		{"injecting", "injecting", ""}, // no arrow → no target
	}
	for _, c := range cases {
		gotState, gotTarget := parseActivity(c.in)
		if gotState != c.wantState || gotTarget != c.wantTarget {
			t.Errorf("parseActivity(%q) = (%q, %q), want (%q, %q)",
				c.in, gotState, gotTarget, c.wantState, c.wantTarget)
		}
	}
}
