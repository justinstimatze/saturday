package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"saturday/watcherclient"
)

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

// newTestOrchestrator returns an Orchestrator with no LLM/network/stage
// dependencies wired — safe for tests exercising only the pure-logic
// decision paths (retype detection, completion-tracking gating). Never
// call Handle/answerAsk/expandAndInject against it; those require a real
// APIKey and hit the network.
func newTestOrchestrator(t *testing.T) *Orchestrator {
	t.Helper()
	// appendFeedbackRec writes to XDG_STATE_HOME/saturday/feedback.jsonl —
	// redirect to a temp dir so tests never touch the real user state dir.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return New(Config{
		StabilityWindow: 5 * time.Second,
		CompletionTTL:   10 * time.Minute,
		MinGrowthBytes:  200,
		MinElapsed:      5 * time.Second,
	})
}

func TestCheckRetype(t *testing.T) {
	o := newTestOrchestrator(t)
	o.recordRecentInject("sid-1", "lucida", "fix the flaky retry test")

	// High overlap with the recorded inject → retype.
	rec, sim, isRetype := o.CheckRetype("sid-1", "fix the flaky retry test please")
	if !isRetype {
		t.Fatalf("expected retype, got sim=%v", sim)
	}
	if rec.Project != "lucida" {
		t.Errorf("rec.Project = %q, want lucida", rec.Project)
	}

	// Low overlap → not a retype.
	if _, _, isRetype := o.CheckRetype("sid-1", "what's the weather"); isRetype {
		t.Error("expected no retype for unrelated prompt")
	}

	// Different session → no match at all, regardless of text overlap.
	if _, _, isRetype := o.CheckRetype("sid-2", "fix the flaky retry test"); isRetype {
		t.Error("expected no retype for a different session")
	}

	// A detected retype should have appended a feedback record.
	root := os.Getenv("XDG_STATE_HOME")
	path := filepath.Join(root, "saturday", "feedback.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected feedback.jsonl to exist after a retype: %v", err)
	}
}

func TestCheckOneInjectTTLExpiry(t *testing.T) {
	o := newTestOrchestrator(t)
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := watcherclient.SessionEntry{JSONLPath: jsonlPath}
	target.State.SessionID = "sid-ttl"
	target.State.Project = "lucida"
	o.trackInject(target, "do the thing", "auto")

	p := o.getPending("sid-ttl")
	if p == nil {
		t.Fatal("expected a pending inject after trackInject")
	}
	// Force it past CompletionTTL.
	p.injectTime = time.Now().Add(-o.cfg.CompletionTTL - time.Second)

	o.checkOneInject(p)

	if o.getPending("sid-ttl") != nil {
		t.Error("expected TTL-expired inject to be dropped")
	}
}

func TestCheckOneInjectTrivialGrowthDropped(t *testing.T) {
	o := newTestOrchestrator(t)
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := watcherclient.SessionEntry{JSONLPath: jsonlPath}
	target.State.SessionID = "sid-trivial"
	target.State.Project = "lucida"
	o.trackInject(target, "do the thing", "auto")

	p := o.getPending("sid-trivial")
	if p == nil {
		t.Fatal("expected a pending inject after trackInject")
	}
	// Size hasn't changed since inject (still "{}\n"), and enough time has
	// passed for both MinElapsed and StabilityWindow — this should be
	// classified as a trivial task and dropped without ever calling the
	// (network-dependent) summarizer.
	p.injectTime = time.Now().Add(-o.cfg.MinElapsed - time.Second)
	p.lastSizeChangeTime = time.Now().Add(-o.cfg.StabilityWindow - time.Second)

	o.checkOneInject(p)

	if o.getPending("sid-trivial") != nil {
		t.Error("expected trivially-grown inject to be dropped")
	}
}

func TestCheckOneInjectGrowthResetsStabilityClock(t *testing.T) {
	o := newTestOrchestrator(t)
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := watcherclient.SessionEntry{JSONLPath: jsonlPath}
	target.State.SessionID = "sid-growth"
	target.State.Project = "lucida"
	o.trackInject(target, "do the thing", "auto")

	p := o.getPending("sid-growth")
	staleChangeTime := time.Now().Add(-time.Hour)
	p.lastSizeChangeTime = staleChangeTime

	// Grow the file past the size checkOneInject last saw.
	if err := os.WriteFile(jsonlPath, []byte(`{"padding":"`+string(make([]byte, 300))+`"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	o.checkOneInject(p)

	if o.getPending("sid-growth") == nil {
		t.Fatal("expected inject to remain tracked after growth, not dropped")
	}
	if !p.lastSizeChangeTime.After(staleChangeTime) {
		t.Error("expected lastSizeChangeTime to reset after detected growth")
	}
	if p.candidateFired {
		t.Error("growth should clear candidateFired, not set it")
	}
}

func TestAccelerateCompletionNoTrackedInject(t *testing.T) {
	o := newTestOrchestrator(t)
	if _, ok := o.AccelerateCompletion("no-such-session"); ok {
		t.Error("expected ok=false for a session with no tracked inject")
	}
}

func TestPendingInjectsSnapshot(t *testing.T) {
	o := newTestOrchestrator(t)
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := watcherclient.SessionEntry{JSONLPath: jsonlPath}
	target.State.SessionID = "sid-snap"
	target.State.Project = "lucida"
	o.trackInject(target, "do the thing", "auto")

	snap := o.PendingInjects()
	if len(snap) != 1 {
		t.Fatalf("expected 1 pending inject, got %d", len(snap))
	}
	if snap[0].SessionID != "sid-snap" || snap[0].Project != "lucida" {
		t.Errorf("unexpected snapshot: %+v", snap[0])
	}

	o.removePending("sid-snap")
	if len(o.PendingInjects()) != 0 {
		t.Error("expected no pending injects after removePending")
	}
}
