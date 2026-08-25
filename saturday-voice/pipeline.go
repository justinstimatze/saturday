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

// dialTimeout bounds how long dialing moshi-server's STT endpoint may
// take. Used to be 10s, sized for a warm Runpod tunnel (Phase 0.5 measured
// connects around 350-850ms) — that reasoning predates the Modal pivot and
// went stale: STT's container scales to zero on the same
// scaledown_window as TTS's, and a real session hit an i/o timeout
// dialing a cold STT container live (2026-08-25), a bug masked in every
// earlier test only because the container happened to already be warm.
// Bumped to match ttsDialTimeout's directly-measured cold-start ceiling —
// STT and TTS are the same cold-start profile (container scheduling +
// moshi-server startup + checkpoint load), not a separately-measured
// number.
const dialTimeout = 60 * time.Second

// ttsDialTimeout: TTS is dialed fresh on every single reply (unlike STT,
// dialed once per session), so it pays the cold-start cost far more
// often — a cold Modal container for it has been directly measured
// taking as long as 42s end-to-end before it's ready to accept the
// WebSocket handshake; a 30s timeout, measured in isolation, was still
// too tight.
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

	// specFired/specSnapshot track a speculative reply already in flight
	// (fired from ActionBeginFlush, before the pause is confirmed) — see
	// runSTTLoop's ActionResponseReady case. Guarded by utteranceMu since
	// they're part of the same "what has the user said so far" state as
	// utteranceBuf.
	specFired    bool
	specSnapshot string

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
		// STT's container scales to zero on the same idle window as TTS's
		// — a cold dial has been directly measured taking 30s+. Unlike
		// speak()'s "warming up voice" message for a cold TTS dial, this
		// first dial previously sent nothing to the client while it was
		// in flight, so a real cold start was indistinguishable from a
		// hang (confirmed live, 2026-08-25: a 31s cold STT dial read as
		// "stuck on connected — listening").
		s.sendControl(map[string]any{"type": "state", "value": "connecting to speech recognition (cold start can take up to a minute)"})
		stt, err := moshiclient.DialSTT(s.sttURL, s.moshiAPIKey, dialTimeout)
		if err != nil {
			log.Printf("dial STT failed: %v", err)
			s.sendControl(map[string]any{"type": "error", "message": "can't reach moshi-server STT, retrying"})
			if !s.sleepOrClientGone(backoff) {
				return fmt.Errorf("dial STT: %w", err)
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		log.Printf("STT dialed OK")
		s.sendControl(map[string]any{"type": "state", "value": "listening"})

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
	framesIn := 0
	sendFailing := false
	for {
		mt, data, err := s.client.ReadMessage()
		if err != nil {
			log.Printf("forwardClientAudio: client read ended after %d frames: %v", framesIn, err)
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
		framesIn++
		if framesIn == 1 {
			log.Printf("forwardClientAudio: first mic frame received from client (%d bytes)", len(data))
		}
		s.connMu.Lock()
		stt := s.stt
		s.connMu.Unlock()
		if stt == nil {
			continue
		}
		if err := stt.SendAudio(bytesToFloat32(data)); err != nil {
			if !sendFailing {
				log.Printf("forwardClientAudio: SendAudio to STT failing: %v", err)
				sendFailing = true
			}
		} else if sendFailing {
			log.Printf("forwardClientAudio: SendAudio to STT recovered")
			sendFailing = false
		}
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
				// Fire preemptive generation here, not at
				// ActionResponseReady: this is the earliest point the
				// turn-taking model itself considers a pause likely (the
				// same prs2>0.6 crossing that will, ~STTDelaySec later,
				// confirm the pause) — starting orchestrator.Handle now
				// overlaps its classify/route/expand network calls with
				// the flush wait instead of paying for both in sequence.
				// See ActionResponseReady below for the staleness check.
				if snap := s.peekUtterance(); strings.TrimSpace(snap) != "" {
					s.utteranceMu.Lock()
					s.specFired = true
					s.specSnapshot = snap
					s.utteranceMu.Unlock()
					log.Printf("turn-taking: firing speculative reply on %q", snap)
					go s.respondSpeculative(snap)
				}
				s.flushSTT()
			case moshiclient.ActionResponseReady:
				utterance := s.takeUtterance()
				s.utteranceMu.Lock()
				specFired, specSnap := s.specFired, s.specSnapshot
				s.specFired, s.specSnapshot = false, ""
				s.utteranceMu.Unlock()
				if specFired && specSnap == utterance {
					// Nothing changed since the speculative call fired —
					// let it run its course; orchestrator.Handle already
					// speaks (or injects) from inside that goroutine, and
					// its own EndResponse/state bookkeeping fires when it
					// returns. Nothing more to do here.
					log.Printf("turn-taking: response ready, speculative reply already covers %q", utterance)
					continue
				}
				if specFired {
					log.Printf("turn-taking: response ready, utterance grew past speculative snapshot (%q -> %q); invalidating and re-firing", specSnap, utterance)
					// Bumps generation (so the speculative call's
					// orchestrator.Handle sees cancelled()==true at its
					// next checkpoint and stops short of speaking/
					// injecting) and closes activeTTS in case the
					// speculative call was fast enough to already be
					// mid-speech on the stale, incomplete input.
					s.cancelActive()
				}
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

// peekUtterance returns the utterance accumulated so far without clearing
// it — used to snapshot a speculative-reply candidate at ActionBeginFlush,
// before the pause is confirmed and takeUtterance() actually claims it.
func (s *session) peekUtterance() string {
	s.utteranceMu.Lock()
	defer s.utteranceMu.Unlock()
	return s.utteranceBuf.String()
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

// respond calls the orchestrator for one utterance whose pause has been
// confirmed (ActionResponseReady already fired). See runReply.
func (s *session) respond(utterance string) {
	s.runReply(utterance, false)
}

// respondSpeculative calls the orchestrator early, on a snapshot of the
// utterance taken at ActionBeginFlush — before the pause is confirmed —
// so its network calls overlap the flush wait instead of following it.
// See runSTTLoop's ActionBeginFlush/ActionResponseReady handling and
// runReply.
func (s *session) respondSpeculative(utterance string) {
	s.runReply(utterance, true)
}

// runReply calls the orchestrator for one utterance. The orchestrator
// speaks any reply (or commits an inject) itself via the Speak callback
// (wired to s.speak) and the cancelled closure passed to Handle —
// runReply's own job is to run Handle and, if this call hasn't been
// superseded by an interrupt or a discovered-stale speculative snapshot
// in the meantime, tell the turn-taking state machine the bot's turn is
// over once Handle returns.
//
// cancelled is built from the generation counter already used for
// interrupt-cancellation (see cancelActive) — it also doubles as the
// staleness check for a speculative call whose snapshot turned out
// incomplete (see runSTTLoop's ActionResponseReady case, which bumps
// generation via cancelActive before firing a fresh respond call).
// Passing it into Handle means a superseded call stops at its next
// checkpoint inside the orchestrator — before speaking or committing an
// inject to a live session — rather than only being noticed here, after
// the fact, once it's too late to un-speak or un-inject.
func (s *session) runReply(utterance string, speculative bool) {
	if strings.TrimSpace(utterance) == "" {
		return
	}

	s.genMu.Lock()
	s.generation++
	myGen := s.generation
	s.genMu.Unlock()
	cancelled := func() bool {
		s.genMu.Lock()
		defer s.genMu.Unlock()
		return s.generation != myGen
	}

	tag := "respond"
	if speculative {
		tag = "respond(speculative)"
	}

	s.sendControl(map[string]any{"type": "state", "value": "thinking"})

	log.Printf("%s: calling orchestrator.Handle(%q)", tag, utterance)
	reply, err := s.orch.Handle(utterance, "expand", "auto", cancelled)
	if err != nil {
		log.Printf("orchestrator.Handle: %v", err)
	} else {
		log.Printf("%s: orchestrator.Handle returned %+v", tag, reply)
	}

	if !cancelled() {
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
	maxAbs := 0.0
	clippedFrames := 0
	// onsetFrames tracks the first 3 frames' peak separately (~240ms at
	// 80ms/frame, roughly "the beginning" the user keeps flagging as
	// still rough after the flat -6dB fix smoothed out the rest) — if
	// the onset genuinely peaks higher than the reply's overall max,
	// that's real data for a targeted onset-only attenuation instead of
	// another blind guess.
	const onsetFrameCount = 3
	onsetMaxAbs := 0.0
	for {
		msg, err := tts.Recv()
		if err != nil {
			// A closed connection (our own cancelActive, or a normal
			// server-side close after Eos) ends the stream — not an
			// error worth surfacing.
			log.Printf("speak: stream ended after %d audio frames (%v) — peak amplitude %.3f (first %d frames: %.3f), %d/%d frames with a sample >1.0 (true clipping)",
				frames, err, maxAbs, onsetFrameCount, onsetMaxAbs, clippedFrames, frames)
			return nil
		}
		switch m := msg.(type) {
		case moshiclient.TTSAudioMessage:
			frames++
			clipped := false
			for _, sample := range m.PCM {
				abs := sample
				if abs < 0 {
					abs = -abs
				}
				if abs > maxAbs {
					maxAbs = abs
				}
				if frames <= onsetFrameCount && abs > onsetMaxAbs {
					onsetMaxAbs = abs
				}
				if abs > 1.0 {
					clipped = true
				}
			}
			if clipped {
				clippedFrames++
			}
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
