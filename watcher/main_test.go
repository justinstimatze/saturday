package main

import "testing"

func TestEncodedHomePrefix(t *testing.T) {
	cases := []struct {
		root string
		want string
	}{
		{"/home/alice/.claude/projects", "-home-alice-"},
		{"/home/bob/.claude/projects", "-home-bob-"},
		{"/Users/jane/.claude/projects", "-Users-jane-"},
		{"/home/alice/.claude/projects/", ""}, // trailing slash → no match → empty
		{"/home/alice/projects", ""},          // not a CC projects root
		{"", ""},
	}
	for _, c := range cases {
		got := encodedHomePrefix(c.root)
		if got != c.want {
			t.Errorf("encodedHomePrefix(%q) = %q, want %q", c.root, got, c.want)
		}
	}
}

func TestDecodeProjectName(t *testing.T) {
	cases := []struct {
		encoded    string
		homePrefix string
		want       string
	}{
		{"-home-alice-Documents-saturday", "-home-alice-", "saturday"},
		{"-home-x-code-foo", "-home-x-", "foo"},
		{"-home-x-src-bar-baz", "-home-x-", "bar-baz"},
		{"-home-alice-misc-thing", "-home-alice-", "misc-thing"},                   // no known anchor → return trailing
		{"-home-other-Documents-foo", "-home-alice-", "-home-other-Documents-foo"}, // wrong prefix → returned verbatim
		{"-home-alice-Documents-", "-home-alice-", ""},
	}
	for _, c := range cases {
		got := decodeProjectName(c.encoded, c.homePrefix)
		if got != c.want {
			t.Errorf("decodeProjectName(%q, %q) = %q, want %q", c.encoded, c.homePrefix, got, c.want)
		}
	}
}
