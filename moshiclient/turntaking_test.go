package moshiclient

import (
	"testing"
	"time"
)

func TestFlushFrameCount(t *testing.T) {
	// ceil(0.5 / 0.08) + 1 = ceil(6.25) + 1 = 7 + 1 = 8
	if got := FlushFrameCount(); got != 8 {
		t.Errorf("FlushFrameCount() = %d, want 8", got)
	}
}

func TestTurnTakingInitialState(t *testing.T) {
	tt := NewTurnTaking()
	if tt.State() != StateWaitingForUser {
		t.Errorf("initial state = %v, want WaitingForUser", tt.State())
	}
}

func TestOnWordTransitionsToUserSpeaking(t *testing.T) {
	tt := NewTurnTaking()
	res := tt.OnWord()
	if res.Interrupted {
		t.Error("expected Interrupted=false when bot wasn't speaking")
	}
	if tt.State() != StateUserSpeaking {
		t.Errorf("state after OnWord = %v, want UserSpeaking", tt.State())
	}
}

func TestOnWordWhileBotSpeakingInterruptsDirectlyToUserSpeaking(t *testing.T) {
	tt := NewTurnTaking()
	tt.state = StateBotSpeaking

	res := tt.OnWord()
	if !res.Interrupted {
		t.Error("expected Interrupted=true when a word arrives during bot_speaking")
	}
	// A real word interrupts straight into user_speaking, skipping
	// waiting_for_user — this is the interruption path, not the VAD
	// threshold path (see TestVADInterruptGoesToWaitingForUser).
	if tt.State() != StateUserSpeaking {
		t.Errorf("state after word-interrupt = %v, want UserSpeaking", tt.State())
	}
}

// skipSteps drives n OnStep calls with a neutral value, consuming the
// stepsToSkip warm-up window without affecting the EMA once it's live.
func skipSteps(tt *TurnTaking, n int, prs2 float64) {
	for i := 0; i < n; i++ {
		tt.OnStep(prs2)
	}
}

func TestFirstTwelveStepsDoNotUpdateEMA(t *testing.T) {
	tt := NewTurnTaking()
	tt.state = StateUserSpeaking
	initial := tt.PausePrediction()

	// Feed 11 steps with an extreme low value — EMA should not move yet.
	for i := 0; i < 11; i++ {
		tt.OnStep(0.0)
	}
	if got := tt.PausePrediction(); got != initial {
		t.Errorf("EMA moved during warm-up window: got %v, want unchanged %v", got, initial)
	}

	// The 12th call consumes the last warm-up slot; the 13th is the first
	// one that actually updates the EMA.
	tt.OnStep(0.0)
	if got := tt.PausePrediction(); got != initial {
		t.Errorf("EMA moved on the 12th (still warm-up) call: got %v, want unchanged %v", got, initial)
	}
	tt.OnStep(0.0)
	if got := tt.PausePrediction(); got == initial {
		t.Error("expected EMA to move on the first post-warm-up step")
	}
}

func TestPauseDetectionTriggersFlushThenResponseReady(t *testing.T) {
	tt := NewTurnTaking()
	// OnWord is how state actually becomes UserSpeaking in real usage —
	// it also resets pausePrediction to 0.0 (see OnWord's doc comment).
	// Setting state directly without going through OnWord would leave
	// pausePrediction at its NewTurnTaking default of 1.0 (matching
	// Unmute's own ExponentialMovingAverage(initial_value=1.0)), which
	// would trip the >0.6 threshold immediately and defeat this test.
	tt.OnWord()
	skipSteps(tt, 12, 0.0) // consume warm-up with a low (speaking) value

	// A run of high prs2 values pushes the EMA above the 0.6 pause
	// threshold — attack_time=0.01s means this takes only a few frames.
	var action StepAction
	for i := 0; i < 10; i++ {
		action = tt.OnStep(1.0)
		if action != ActionNone {
			break
		}
	}
	if action != ActionBeginFlush {
		t.Fatalf("action = %v, want ActionBeginFlush", action)
	}
	if tt.State() != StateUserSpeaking {
		t.Errorf("state during flush = %v, want still UserSpeaking", tt.State())
	}

	// Keep stepping until the flush window (STTDelaySec worth of frames)
	// elapses — should eventually report ActionResponseReady and flip to
	// BotSpeaking.
	var got StepAction
	for i := 0; i < FlushFrameCount()+2; i++ {
		got = tt.OnStep(1.0)
		if got == ActionResponseReady {
			break
		}
		if got != ActionNone {
			t.Fatalf("unexpected action mid-flush: %v", got)
		}
	}
	if got != ActionResponseReady {
		t.Fatalf("flush never completed within %d frames", FlushFrameCount()+2)
	}
	if tt.State() != StateBotSpeaking {
		t.Errorf("state after ActionResponseReady = %v, want BotSpeaking", tt.State())
	}
}

func TestVADInterruptGoesToWaitingForUser(t *testing.T) {
	tt := NewTurnTaking()
	tt.state = StateBotSpeaking
	tt.pausePrediction.Value = 1.0 // start high so it has to fall below 0.4
	tt.sessionStart = time.Now().Add(-UninterruptibleByVADTimeSec * time.Second).Add(-time.Second)

	// Drive the EMA down below 0.4 with low prs2 values (past the 12-step
	// warm-up, which was already consumed conceptually — set stepsToSkip
	// to 0 directly since this test only cares about the threshold logic).
	tt.stepsToSkip = 0
	var action StepAction
	for i := 0; i < 50; i++ {
		action = tt.OnStep(0.0)
		if action == ActionInterrupt {
			break
		}
	}
	if action != ActionInterrupt {
		t.Fatal("expected ActionInterrupt once pause_prediction fell below 0.4")
	}
	if tt.State() != StateWaitingForUser {
		t.Errorf("state after VAD interrupt = %v, want WaitingForUser", tt.State())
	}
}

func TestVADInterruptGuardedDuringUninterruptibleWindow(t *testing.T) {
	tt := NewTurnTaking()
	tt.state = StateBotSpeaking
	tt.pausePrediction.Value = 1.0
	tt.stepsToSkip = 0
	// sessionStart is "now" (default from NewTurnTaking) — well inside the
	// 3s uninterruptible window.

	for i := 0; i < 50; i++ {
		if action := tt.OnStep(0.0); action == ActionInterrupt {
			t.Fatal("VAD interrupt fired inside the uninterruptible window")
		}
	}
	if tt.State() != StateBotSpeaking {
		t.Errorf("state = %v, want still BotSpeaking (no interrupt should have fired)", tt.State())
	}
}

func TestEndResponseReturnsToWaitingForUser(t *testing.T) {
	tt := NewTurnTaking()
	tt.state = StateBotSpeaking
	tt.EndResponse()
	if tt.State() != StateWaitingForUser {
		t.Errorf("state after EndResponse = %v, want WaitingForUser", tt.State())
	}
}

func TestNoPauseDetectionWhileWaitingForUser(t *testing.T) {
	tt := NewTurnTaking()
	// state is WaitingForUser (never got a word) — a high prs2 shouldn't
	// trigger a flush, since determine_pause only applies during
	// user_speaking.
	skipSteps(tt, 12, 1.0)
	if action := tt.OnStep(1.0); action != ActionNone {
		t.Errorf("action = %v, want ActionNone while WaitingForUser", action)
	}
}
