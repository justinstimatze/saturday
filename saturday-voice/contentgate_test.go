package main

import "testing"

func TestHasMeaningfulContent(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"*", false},
		{"...", false},
		{"a", false}, // below minMeaningfulChars
		{"ok", true},
		{"no", true},
		{"* actually check the logs", true},
		{"héllo", true}, // unicode letters count
	}
	for _, c := range cases {
		if got := hasMeaningfulContent(c.in); got != c.want {
			t.Errorf("hasMeaningfulContent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
