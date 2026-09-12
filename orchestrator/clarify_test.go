package orchestrator

import (
	"testing"
	"time"
)

func TestIsAffirmative(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"yes", true},
		{"Yes.", true},
		{"  yeah  ", true},
		{"yup!", true},
		{"correct", true},
		{"no", false},
		{"nope", false},
		{"", false},
		{"lucida", false},
		{"yes please do it", false}, // fixed vocabulary, not a prefix match
	}
	for _, c := range cases {
		if got := isAffirmative(c.in); got != c.want {
			t.Errorf("isAffirmative(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestResolveClarifyNoPending(t *testing.T) {
	o := New(Config{})
	_, _, handled := o.resolveClarify("yes", func() bool { return false })
	if handled {
		t.Error("handled = true with no pending clarify, want false")
	}
}

func TestResolveClarifyExpired(t *testing.T) {
	o := New(Config{})
	o.clarify = &pendingClarify{
		utterance: "let's look at the backlog",
		expiresAt: time.Now().Add(-time.Second),
	}
	_, _, handled := o.resolveClarify("yes", func() bool { return false })
	if handled {
		t.Error("handled = true for an expired clarify, want false")
	}
	if o.clarify != nil {
		t.Error("expired clarify should be cleared")
	}
}

func TestResolveClarifyNoReplyDropsSilently(t *testing.T) {
	o := New(Config{
		Speak: func(string) error {
			t.Fatal("Speak should not be called resolving a no/unrecognized clarify reply")
			return nil
		},
	})
	o.clarify = &pendingClarify{
		utterance: "let's look at the backlog",
		expiresAt: time.Now().Add(time.Minute),
	}
	dec, err, handled := o.resolveClarify("banana", func() bool { return false })
	if !handled {
		t.Error("handled = false for an unrecognized reply, want true")
	}
	if dec != nil || err != nil {
		t.Errorf("dec=%v err=%v, want nil, nil", dec, err)
	}
	if o.clarify != nil {
		t.Error("clarify should be cleared after a no/unrecognized reply")
	}
}

func TestBeginClarifySkipsWhenAlreadyCancelled(t *testing.T) {
	o := New(Config{
		Speak: func(string) error {
			t.Fatal("Speak should not be called for an already-cancelled clarify — asking an obsolete question is worse than the silent drop it replaces")
			return nil
		},
	})
	if err := o.beginClarify("let's look at the backlog", "expand", "auto", "lucida", func() bool { return true }); err != nil {
		t.Errorf("beginClarify returned %v, want nil", err)
	}
	if o.clarify != nil {
		t.Error("clarify should not be set when the call was already cancelled")
	}
}

// Not unit-tested here: HandleSpeculative never resolving/opening a
// clarify window end-to-end (the actual race a /check-plan review
// caught). handleTop's own gate (o.cfg.Clarify && clarifyEnabled before
// calling resolveClarify) is trivially correct by inspection — the real
// risk was always the SECOND gate, inside handle()'s router-gate site,
// which also checks clarifyEnabled before calling beginClarify (see
// handle's doc comment). Proving that gate fires correctly means running
// handleTop through to the router gate, which means a real classify call
// first — the same reason orchestrator_test.go never calls
// Handle/expandAndInject directly (no mock seam for llm.RunClassify/
// RunRoute exists, and a missing/fake APIKey either hits the real network
// or fails in a way that isn't a meaningful assertion). Covered by the
// live-manual-test checklist's explicit speculative-race step instead.
