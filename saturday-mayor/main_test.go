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

func TestStripWakeWord(t *testing.T) {
	cases := []struct {
		in       string
		wantRest string
		wantHit  bool
	}{
		{"saturday what's up", "what's up", true},
		{"Saturday, ping me", "ping me", true},
		{"hey saturday open the lucida tests", "open the lucida tests", true},
		{"HEY SATURDAY!", "", true},                       // bare wake word after trim
		{"saturday", "", true},                            // bare
		{"saturdayfile.go", "saturdayfile.go", false},     // glued to non-separator
		{"fix saturdays bug", "fix saturdays bug", false}, // not a prefix
		{"  hey saturday  ping  ", "ping", true},          // leading/trailing trim
		{"", "", false},
	}
	for _, c := range cases {
		gotRest, gotHit := stripWakeWord(c.in)
		if gotRest != c.wantRest || gotHit != c.wantHit {
			t.Errorf("stripWakeWord(%q) = (%q, %v), want (%q, %v)",
				c.in, gotRest, gotHit, c.wantRest, c.wantHit)
		}
	}
}
