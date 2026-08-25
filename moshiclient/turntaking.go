package moshiclient

import (
	"math"
	"sync"
	"time"
)

// UninterruptibleByVADTimeSec is how long after a session starts the VAD-
// threshold interruption path stays disabled — ported from Unmute's
// UNINTERRUPTIBLE_BY_VAD_TIME_SEC. On some setups echo cancellation takes
// a moment to kick in, so the STT can briefly hear the bot's own TTS
// output and misfire an interruption; a real word from the user can still
// interrupt during this window (see OnWord), only the raw VAD threshold
// is gated. This is a one-time guard from session start, not a per-turn
// grace window.
const UninterruptibleByVADTimeSec = 3.0

// ConversationState mirrors Unmute's own three-state model
// (unmute/llm/chatbot.py's ConversationState), simplified: Saturday
// doesn't need Unmute's chat-history bookkeeping since each reply comes
// from a single orchestrator.Handle call, not a stateful multi-turn LLM
// conversation.
type ConversationState int

const (
	StateWaitingForUser ConversationState = iota
	StateUserSpeaking
	StateBotSpeaking
)

func (s ConversationState) String() string {
	switch s {
	case StateWaitingForUser:
		return "waiting_for_user"
	case StateUserSpeaking:
		return "user_speaking"
	case StateBotSpeaking:
		return "bot_speaking"
	default:
		return "unknown"
	}
}

// StepAction tells the caller what to do after feeding OnStep one STT Step
// message.
type StepAction int

const (
	// ActionNone: nothing to do.
	ActionNone StepAction = iota
	// ActionBeginFlush: a pause was just detected. Send FlushFrameCount()
	// zero-padding frames to the STT connection (one per subsequent
	// OnStep call, at the normal audio cadence) so moshi-server's STT
	// catches up to real time before its output is treated as final.
	ActionBeginFlush
	// ActionResponseReady: the post-pause flush finished. Call
	// BeginResponse(), then generate and speak a reply.
	ActionResponseReady
	// ActionInterrupt: the bot was speaking and the raw VAD threshold
	// (not a real word — see OnWord for that path) just fired. Cancel any
	// in-flight response/TTS immediately.
	ActionInterrupt
)

func (a StepAction) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionBeginFlush:
		return "begin_flush"
	case ActionResponseReady:
		return "response_ready"
	case ActionInterrupt:
		return "interrupt"
	default:
		return "unknown"
	}
}

// WordResult is returned by OnWord.
type WordResult struct {
	// Interrupted is true if the bot was speaking and this word just
	// interrupted it. The caller must cancel any in-flight
	// orchestrator.Handle/TTS output — a real word interrupts
	// unconditionally, independent of the pause-prediction threshold.
	Interrupted bool
}

// TurnTaking is the pause-detection/barge-in state machine, a direct port
// of Unmute's own logic (unmute_handler.py's determine_pause/interrupt_bot,
// chatbot.py's conversation_state) read from source rather than
// reimplemented from a description. It owns no network I/O — callers
// drive it from their STT receive loop (OnStep/OnWord) and their own
// response lifecycle (BeginResponse/EndResponse), and act on its returned
// StepAction/WordResult.
//
// Deliberately NOT ported: Unmute's long-silence check-in
// (detect_long_silence) and goodbye-detection (check_for_bot_goodbye) —
// both are UX niceties for Unmute's general-chatbot use case with no
// equivalent in Saturday's command-oriented voice interaction model.
type TurnTaking struct {
	mu sync.Mutex

	state ConversationState

	// pausePrediction smooths moshi-server STT's own semantic
	// pause-prediction score (Step.Prs[2]). Attack/release both 0.01s —
	// Unmute's own tuning, validated live over a real WAN hop in Phase
	// 0.5 and left unchanged (see project memory).
	pausePrediction *EMA
	// stepsToSkip: the first 12 Step messages are too noisy to trust —
	// ignored for EMA purposes, matching Unmute's n_steps_to_wait.
	stepsToSkip int

	// currentSTTTime mirrors moshi-server's own advancing clock as seen
	// through Step messages, starting at -STTDelaySec exactly as Unmute's
	// SpeechToText does.
	currentSTTTime float64
	flushing       bool
	flushEndTime   float64

	sessionStart time.Time
}

// NewTurnTaking constructs a TurnTaking in the WaitingForUser state.
func NewTurnTaking() *TurnTaking {
	return &TurnTaking{
		state:           StateWaitingForUser,
		pausePrediction: NewEMA(0.01, 0.01, 1.0),
		stepsToSkip:     12,
		currentSTTTime:  -STTDelaySec,
		sessionStart:    time.Now(),
	}
}

// FlushFrameCount returns how many SamplesPerFrame-sized zero-padding
// frames to send to STT after ActionBeginFlush — ported from Unmute's own
// num_frames computation (ceil(STTDelaySec/FrameTimeSec) + 1 safety
// margin).
func FlushFrameCount() int {
	return int(math.Ceil(STTDelaySec/FrameTimeSec)) + 1
}

// State returns the current conversation state.
func (tt *TurnTaking) State() ConversationState {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.state
}

// PausePrediction returns the current smoothed pause-prediction value,
// for logging/debugging.
func (tt *TurnTaking) PausePrediction() float64 {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.pausePrediction.Value
}

// OnWord should be called whenever the STT client yields a
// STTWordMessage. Whether this starts a fresh user utterance is derived
// internally from the current state (mirrors Unmute's own
// add_chat_message_delta: a new message begins whenever the caller wasn't
// already mid-utterance), so callers don't need to track that themselves.
func (tt *TurnTaking) OnWord() WordResult {
	tt.mu.Lock()
	defer tt.mu.Unlock()

	interrupted := tt.state == StateBotSpeaking
	isNewMessage := tt.state != StateUserSpeaking
	if isNewMessage {
		// Ensure we don't stop after the first word if the pause
		// detector didn't have time to react to the new utterance yet.
		tt.pausePrediction.Value = 0.0
	}
	tt.state = StateUserSpeaking
	return WordResult{Interrupted: interrupted}
}

// OnStep should be called whenever the STT client yields a
// STTStepMessage, passing its Prs[2] value (moshi-server's own semantic
// pause-prediction score for this frame).
func (tt *TurnTaking) OnStep(prs2 float64) StepAction {
	tt.mu.Lock()
	defer tt.mu.Unlock()

	tt.currentSTTTime += FrameTimeSec

	if tt.stepsToSkip > 0 {
		tt.stepsToSkip--
	} else {
		tt.pausePrediction.Update(FrameTimeSec, prs2)
	}

	if tt.flushing {
		if tt.currentSTTTime > tt.flushEndTime {
			tt.flushing = false
			// bot_speaking begins the moment we decide to respond, not
			// once audio actually starts — matches Unmute's own
			// _generate_response appending an empty assistant message
			// before awaiting anything.
			tt.state = StateBotSpeaking
			return ActionResponseReady
		}
		return ActionNone
	}

	if tt.state == StateUserSpeaking && tt.pausePrediction.Value > 0.6 {
		tt.flushing = true
		tt.flushEndTime = tt.currentSTTTime + STTDelaySec
		return ActionBeginFlush
	}

	if tt.state == StateBotSpeaking &&
		tt.pausePrediction.Value < 0.4 &&
		time.Since(tt.sessionStart).Seconds() > UninterruptibleByVADTimeSec {
		tt.state = StateWaitingForUser
		return ActionInterrupt
	}

	return ActionNone
}

// BeginResponse is a no-op state assertion for callers that want to be
// explicit about the transition ActionResponseReady already performed —
// state is already StateBotSpeaking by the time OnStep returns
// ActionResponseReady. Provided for symmetry with EndResponse; most
// callers can ignore it.
func (tt *TurnTaking) BeginResponse() {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.state = StateBotSpeaking
}

// EndResponse marks a full, uninterrupted response as complete —
// transitions back to WaitingForUser. Call this once TTS has finished
// speaking the reply.
func (tt *TurnTaking) EndResponse() {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.state = StateWaitingForUser
}
