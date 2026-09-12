package main

import (
	"errors"
	"testing"
	"time"

	"saturday/moshiclient"
)

// scriptedSTTConn's Recv returns msgs in order, then blocks until Close is
// called (mirroring fakeSTTConn's pattern in dial_test.go) — lets a test
// drive runSTTLoop's turn-taking handlers through a deterministic,
// pre-scripted sequence of STT messages.
type scriptedSTTConn struct {
	msgs    []any
	idx     int
	closeCh chan struct{}
}

func newScriptedSTTConn(msgs []any) *scriptedSTTConn {
	return &scriptedSTTConn{msgs: msgs, closeCh: make(chan struct{})}
}

func (f *scriptedSTTConn) SendAudio([]float32) error { return nil }

func (f *scriptedSTTConn) Recv() (any, error) {
	if f.idx < len(f.msgs) {
		m := f.msgs[f.idx]
		f.idx++
		return m, nil
	}
	<-f.closeCh
	return nil, errors.New("scripted stt: script exhausted, connection closed")
}

func (f *scriptedSTTConn) Close() error {
	select {
	case <-f.closeCh:
	default:
		close(f.closeCh)
	}
	return nil
}

// TestRunSTTLoopSkipsNonContentUtterance is the regression test for the
// live "*" bug (2026-08-26): a single non-content STT token with high
// pause-confidence must never reach orchestrator.Handle. s.orch is left
// nil deliberately — if hasMeaningfulContent's gate is missing or broken,
// the wrongly-fired `go s.respond(...)`/`go s.respondSpeculative(...)`
// goroutine dereferences a nil *orchestrator.Orchestrator and panics,
// crashing this test loudly rather than failing a quiet assertion.
//
// The word/step sequence is exactly moshiclient/turntaking_test.go's own
// TestPauseDetectionTriggersFlushThenResponseReady recipe — that's the
// deterministic warm-up/threshold-crossing shape needed to reach
// ActionBeginFlush then ActionResponseReady at all, independent of what
// the transcribed content actually is.
func TestRunSTTLoopSkipsNonContentUtterance(t *testing.T) {
	var msgs []any
	msgs = append(msgs, moshiclient.STTWordMessage{Text: "*"})
	for i := 0; i < 12; i++ {
		msgs = append(msgs, moshiclient.STTStepMessage{Prs: []float64{0, 0, 0.0}})
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, moshiclient.STTStepMessage{Prs: []float64{0, 0, 1.0}})
	}
	for i := 0; i < moshiclient.FlushFrameCount()+2; i++ {
		msgs = append(msgs, moshiclient.STTStepMessage{Prs: []float64{0, 0, 1.0}})
	}

	conn := newScriptedSTTConn(msgs)
	s := &session{
		stt: conn,
		tt:  moshiclient.NewTurnTaking(),
		// orch, client, acks all deliberately nil — see doc comment above.
	}

	done := make(chan error, 1)
	go func() { done <- s.runSTTLoop() }()

	select {
	case <-done:
		t.Fatal("runSTTLoop returned before the script was exhausted — script/timing mismatch")
	case <-time.After(200 * time.Millisecond):
		// Give any wrongly-fired goroutine (go s.respond/respondSpeculative)
		// a real chance to run and panic before declaring success.
	}
	conn.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runSTTLoop returned nil error, want the scripted close error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runSTTLoop did not return after the scripted connection closed")
	}

	if got := s.turnTaking().State(); got != moshiclient.StateWaitingForUser {
		t.Errorf("turn-taking state after a skipped non-content response = %v, want StateWaitingForUser (EndResponse must still fire on the skip path)", got)
	}
}
