package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"saturday/moshiclient"
	"saturday/orchestrator"
)

// dialTimeout bounds how long dialing moshi-server's STT endpoint may take
// — generous enough for a real WAN hop (Phase 0.5 measured connects around
// 350-850ms over a Runpod tunnel) without hanging indefinitely on a dead
// pod.
const dialTimeout = 10 * time.Second

// ttsDialTimeout is longer than dialTimeout: unlike the STT link (dialed
// once per session and typically already warm by the time speech starts),
// TTS is dialed fresh on every single reply, and a cold Modal container
// for it has been directly measured taking as long as 42s end-to-end
// (container scheduling + moshi-server startup + checkpoint/voice-file
// load) before it's ready to accept the WebSocket handshake — a 30s
// timeout, measured in isolation, was still too tight.
const ttsDialTimeout = 60 * time.Second

// session owns one client connection's lifecycle: dial STT/TTS, run the
// turn-taking loop, call the orchestrator, stream audio both ways. One
// session per connected client — saturday-voice itself is otherwise
// stateless per-connection (all cross-session voice-routing state lives
// in the shared *orchestrator.Orchestrator).
type session struct {
	orch *orchestrator.Orchestrator

	client   *websocket.Conn
	clientMu sync.Mutex // gorilla/websocket requires a single writer

	sttURL, ttsURL, moshiAPIKey string
	voice                       moshiclient.TTSVoice

	// connMu guards stt/tt together — run() swaps both as a pair on every
	// reconnect, and forwardClientAudio (a single goroutine that outlives
	// any one STT connection) reads stt from outside run()'s own
	// goroutine, which is what actually needs the lock. runSTTLoop reads
	// both directly, unlocked: it always runs synchronously from the same
	// goroutine that just set them, sequenced after that write by Go's
	// memory model, with no other writer active while it runs — see run().
	connMu sync.Mutex
	stt    *moshiclient.STTClient
	tt     *moshiclient.TurnTaking

	// clientDone is closed exactly once, by forwardClientAudio, when the
	// client WebSocket itself dies — the one reliable signal for "stop
	// reconnecting, nobody's listening" (an STT-side drop with the client
	// still live looks identical from run()'s perspective except for this
	// channel).
	clientDone chan struct{}

	utteranceMu  sync.Mutex
	utteranceBuf strings.Builder

	// genMu guards generation (bumped on every interrupt, so a stale
	// respond() call can tell it's been superseded and skip its
	// now-irrelevant EndResponse) and activeTTS (the current turn's TTS
	// connection, closed on interrupt to stop it mid-stream — orchestrator
	// .Handle itself has no cancellation support, so this is the actual
	// mechanism behind "interrupt": we can't abort an in-flight Handle
	// call, only discard its result and stop it from being spoken).
	genMu      sync.Mutex
	generation int
	activeTTS  *moshiclient.TTSClient
}

func newSession(orch *orchestrator.Orchestrator, client *websocket.Conn, sttURL, ttsURL, moshiAPIKey string, voice moshiclient.TTSVoice) *session {
	return &session{
		orch:        orch,
		client:      client,
		sttURL:      sttURL,
		ttsURL:      ttsURL,
		moshiAPIKey: moshiAPIKey,
		voice:       voice,
		tt:          moshiclient.NewTurnTaking(),
	}
}

// run dials STT and drives the session until the client disconnects.
// Blocks; call from its own goroutine per accepted client.
//
// The STT link to a rented, GPU-contended, WAN-connected moshi-server pod
// is the one that will actually drop in practice — Phase 0.5 already
// produced a `Sending after closing is not allowed` WS error live, and
// the persona review named this gap explicitly. On an unexpected STT
// disconnect (the client is still connected), reconnect with capped
// exponential backoff, mirroring stageclient.Client.Run's own pattern —
// the precedent the plan calls out. Reconnecting resets turn-taking to
// WaitingForUser rather than trying to resume mid-utterance: whatever the
// user was saying is lost and they'll need to repeat it, which is the
// honest, simple choice named in the plan (discard-and-restart, not
// silent-resume) rather than a seamless resume this pass doesn't attempt.
//
// forwardClientAudio runs once, for the session's whole lifetime, rather
// than being restarted per reconnect attempt: an earlier draft spawned a
// fresh forwarder bound to each dialed STT connection and raced it against
// the next reconnect with only a 50ms grace window to prove the old one
// had actually exited — client.ReadMessage() only unblocks when the next
// audio frame arrives, which isn't bounded by 50ms, so two forwarders could
// both hold a live ReadMessage() call on the same *websocket.Conn at once
// (gorilla/websocket allows exactly one concurrent reader). A single
// long-lived forwarder that looks up the current STT connection under
// connMu removes the lifecycle race entirely instead of tuning the window.
func (s *session) run() error {
	backoff := time.Second
	const maxBackoff = 16 * time.Second

	s.clientDone = make(chan struct{})
	go s.forwardClientAudio()

	for {
		stt, err := moshiclient.DialSTT(s.sttURL, s.moshiAPIKey, dialTimeout)
		if err != nil {
			s.sendControl(map[string]any{"type": "error", "message": "can't reach moshi-server STT, retrying"})
			if !s.sleepOrClientGone(backoff) {
				return fmt.Errorf("dial STT: %w", err)
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		s.connMu.Lock()
		s.stt = stt
		s.tt = moshiclient.NewTurnTaking() // fresh state — see doc comment
		s.connMu.Unlock()
		backoff = time.Second // reset once a connection succeeds

		loopErr := s.runSTTLoop()
		_ = stt.Close()

		// clientDone is only ever closed by forwardClientAudio, and only
		// once its own client.ReadMessage() has actually errored — unlike
		// the old grace-window guess, this is a direct signal, not an
		// inference.
		select {
		case <-s.clientDone:
			return loopErr
		default:
		}

		s.sendControl(map[string]any{"type": "state", "value": "reconnecting"})
		if !s.sleepOrClientGone(backoff) {
			return loopErr
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// forwardClientAudio reads mic audio from the client for the session's
// whole lifetime and forwards each frame to whichever STT connection is
// current, under connMu. A frame arriving during a reconnect gap (stt nil,
// or a SendAudio error on a connection that's mid-close) is dropped
// silently — turn-taking already resets to WaitingForUser on reconnect
// (see run()'s doc comment), so losing a few frames right at the boundary
// doesn't change what the user needs to do (repeat themselves either way).
// On the client connection itself dying, closes the current STT connection
// (to unblock whatever Recv() run()'s STT loop is blocked in) and
// clientDone (to tell run() to stop reconnecting), then returns.
func (s *session) forwardClientAudio() {
	defer close(s.clientDone)
	for {
		mt, data, err := s.client.ReadMessage()
		if err != nil {
			s.connMu.Lock()
			stt := s.stt
			s.connMu.Unlock()
			if stt != nil {
				_ = stt.Close()
			}
			return
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		s.connMu.Lock()
		stt := s.stt
		s.connMu.Unlock()
		if stt == nil {
			continue
		}
		_ = stt.SendAudio(bytesToFloat32(data))
	}
}

// turnTaking returns the current turn-taking state machine under connMu —
// for callers outside run()'s own goroutine (respond(), specifically),
// which can still be in flight when a reconnect swaps s.tt out from under
// it. See connMu's doc comment on session.
func (s *session) turnTaking() *moshiclient.TurnTaking {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.tt
}

// sleepOrClientGone waits d, but returns early (false) if the client
// connection has already died — no point backing off a reconnect nobody's
// listening for. clientDone is the same reliable signal forwardClientAudio
// closes on a real client-read error, not a separate liveness probe.
func (s *session) sleepOrClientGone(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-s.clientDone:
		return false
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	if cur >= max {
		return max
	}
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

// runSTTLoop reads STT messages and drives the turn-taking state machine.
func (s *session) runSTTLoop() error {
	for {
		msg, err := s.stt.Recv()
		if err != nil {
			return err
		}
		switch m := msg.(type) {
		case moshiclient.STTWordMessage:
			// moshi-server's STT sends an empty first message; Unmute's
			// own client skips it too so it doesn't spuriously flip
			// waiting_for_user into user_speaking before the user has
			// actually said anything.
			if m.Text == "" {
				continue
			}
			log.Printf("stt word: %q", m.Text)
			res := s.tt.OnWord()
			if res.Interrupted {
				s.cancelActive()
			}
			s.appendUtterance(m.Text)

		case moshiclient.STTStepMessage:
			var prs2 float64
			if len(m.Prs) > 2 {
				prs2 = m.Prs[2]
			}
			switch action := s.tt.OnStep(prs2); action {
			case moshiclient.ActionBeginFlush:
				log.Printf("turn-taking: begin flush (prs2=%.3f)", prs2)
				s.flushSTT()
			case moshiclient.ActionResponseReady:
				utterance := s.takeUtterance()
				log.Printf("turn-taking: response ready, utterance=%q", utterance)
				go s.respond(utterance)
			case moshiclient.ActionInterrupt:
				log.Printf("turn-taking: interrupt (prs2=%.3f)", prs2)
				s.cancelActive()
			}

		case moshiclient.STTErrorMessage:
			return fmt.Errorf("stt error: %s", m.Message)
		}
	}
}

// flushSTT pads the STT stream with silence after a detected pause so
// moshi-server catches up to real time before its output is treated as
// final — see moshiclient.FlushFrameCount's doc comment.
func (s *session) flushSTT() {
	zero := make([]float32, moshiclient.SamplesPerFrame)
	for i := 0; i < moshiclient.FlushFrameCount(); i++ {
		if err := s.stt.SendAudio(zero); err != nil {
			return
		}
	}
}

func (s *session) appendUtterance(text string) {
	s.utteranceMu.Lock()
	defer s.utteranceMu.Unlock()
	if s.utteranceBuf.Len() > 0 {
		s.utteranceBuf.WriteString(" ")
	}
	s.utteranceBuf.WriteString(text)
}

func (s *session) takeUtterance() string {
	s.utteranceMu.Lock()
	defer s.utteranceMu.Unlock()
	t := s.utteranceBuf.String()
	s.utteranceBuf.Reset()
	return t
}

// cancelActive bumps the generation counter (so any in-flight respond()
// call knows its result is now stale) and closes the current turn's TTS
// connection if one is open, which stops moshi-server mid-synthesis and
// unblocks speak()'s receive loop.
func (s *session) cancelActive() {
	s.genMu.Lock()
	s.generation++
	tts := s.activeTTS
	s.activeTTS = nil
	s.genMu.Unlock()
	if tts != nil {
		_ = tts.Close()
	}
	s.sendControl(map[string]any{"type": "interrupted"})
}

// respond calls the orchestrator for one utterance. The orchestrator
// speaks any reply itself via the Speak callback (wired to s.speak) —
// respond's own job is just to run Handle and, if this call hasn't been
// superseded by an interrupt in the meantime, tell the turn-taking state
// machine the bot's turn is over once Handle returns.
func (s *session) respond(utterance string) {
	if strings.TrimSpace(utterance) == "" {
		return
	}

	s.genMu.Lock()
	s.generation++
	myGen := s.generation
	s.genMu.Unlock()

	s.sendControl(map[string]any{"type": "state", "value": "thinking"})

	log.Printf("respond: calling orchestrator.Handle(%q)", utterance)
	reply, err := s.orch.Handle(utterance, "expand", "auto")
	if err != nil {
		log.Printf("orchestrator.Handle: %v", err)
	} else {
		log.Printf("respond: orchestrator.Handle returned %+v", reply)
	}

	s.genMu.Lock()
	current := myGen == s.generation
	s.genMu.Unlock()
	if current {
		s.turnTaking().EndResponse()
		s.sendControl(map[string]any{"type": "state", "value": "listening"})
	}
}

// speak wires orchestrator.Config.Speak: dial a fresh TTS connection,
// send the text, and stream the resulting audio to the client. Runs
// synchronously on whatever goroutine called orchestrator.Handle (per
// Phase 1a's design, Handle itself is synchronous/single-flight — see
// orchestrator's own doc comment).
func (s *session) speak(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		log.Printf("speak: empty text, skipping")
		return nil
	}
	log.Printf("speak: dialing TTS for %q", text)
	// A cold TTS container has been measured taking up to ~40s to become
	// reachable (container scheduling + moshi-server startup + checkpoint
	// load) — "thinking" undersells that wait, so give it its own signal.
	s.sendControl(map[string]any{"type": "state", "value": "warming up voice (cold start can take up to a minute)"})
	tts, err := moshiclient.DialTTS(s.ttsURL, s.moshiAPIKey, s.voice, 1.5, ttsDialTimeout)
	if err != nil {
		log.Printf("speak: dial TTS failed: %v", err)
		return fmt.Errorf("dial TTS: %w", err)
	}

	s.genMu.Lock()
	s.activeTTS = tts
	s.genMu.Unlock()
	defer func() {
		s.genMu.Lock()
		if s.activeTTS == tts {
			s.activeTTS = nil
		}
		s.genMu.Unlock()
		_ = tts.Close()
	}()

	if err := tts.SendText(text); err != nil {
		return fmt.Errorf("send TTS text: %w", err)
	}
	if err := tts.SendEOS(); err != nil {
		return fmt.Errorf("send TTS EOS: %w", err)
	}

	s.sendControl(map[string]any{"type": "state", "value": "speaking"})

	frames := 0
	for {
		msg, err := tts.Recv()
		if err != nil {
			// A closed connection (our own cancelActive, or a normal
			// server-side close after Eos) ends the stream — not an
			// error worth surfacing.
			log.Printf("speak: stream ended after %d audio frames (%v)", frames, err)
			return nil
		}
		switch m := msg.(type) {
		case moshiclient.TTSAudioMessage:
			frames++
			if err := s.sendAudio(m.PCM); err != nil {
				log.Printf("speak: sendAudio to client failed: %v", err)
			}
		case moshiclient.TTSErrorMessage:
			log.Printf("speak: tts error: %s", m.Message)
			return fmt.Errorf("tts error: %s", m.Message)
		}
	}
}

// sendAudio writes one frame of synthesized PCM to the client as a binary
// WS message.
func (s *session) sendAudio(pcm []float64) error {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	return s.client.WriteMessage(websocket.BinaryMessage, float64PCMToBytes(pcm))
}

// sendControl writes a small JSON control event to the client as a text
// WS message — kept on a separate message type from raw audio frames so
// the client can distinguish "here is audio to play" from "clear your
// playback queue now" / a status update, without needing an envelope on
// every audio frame.
func (s *session) sendControl(evt map[string]any) {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	_ = s.client.WriteJSON(evt)
}
