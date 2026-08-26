package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"saturday/moshiclient"
)

// This file covers the dial/reconnect/control-message layer named as a
// real coverage gap live (2026-08-25): the stuck "connecting..." message
// regression, the reconnect-vs-give-up decision, and the generation-based
// cancellation the preemptive-reply mechanism depends on. It exercises the
// real sendControl/run()/runSTTLoop code path over a real client
// WebSocket, with only the moshi-server leg faked (see sttConn/ttsConn on
// session) — moshi-server's own wire protocol already has fixture-driven
// coverage in moshiclient.

// fakeSTTConn's Recv blocks until Close is called, then returns an error —
// mirroring a real *moshiclient.STTClient whose underlying connection was
// closed out from under a pending Recv(). Pre-closing closeCh (see the
// STT-loop-error tests below) makes Recv return immediately instead,
// simulating an already-dead connection.
type fakeSTTConn struct {
	closeCh   chan struct{}
	closeOnce sync.Once
}

func newFakeSTTConn() *fakeSTTConn { return &fakeSTTConn{closeCh: make(chan struct{})} }

func (f *fakeSTTConn) SendAudio([]float32) error { return nil }
func (f *fakeSTTConn) Recv() (any, error) {
	<-f.closeCh
	return nil, errors.New("fake stt: connection closed")
}
func (f *fakeSTTConn) Close() error {
	f.closeOnce.Do(func() { close(f.closeCh) })
	return nil
}

// dialScript scripts sttConn.dialSTT's return value per call, by call
// index (0-based) — lets a test express "fails once, then succeeds" or
// "always fails" without a stateful mock framework.
type dialScript struct {
	mu    sync.Mutex
	calls int32
	fn    func(call int) (sttConn, error)
}

func (d *dialScript) dial(string, string, time.Duration) (sttConn, error) {
	call := int(atomic.AddInt32(&d.calls, 1)) - 1
	return d.fn(call)
}

func (d *dialScript) callCount() int {
	return int(atomic.LoadInt32(&d.calls))
}

// newWSPair spins up a real in-process WebSocket server and returns both
// ends: serverConn is what session.client would be in production
// (upgrader.Upgrade's result — see main.go's serveWS), clientConn is the
// "browser" side the test reads control/audio messages from.
func newWSPair(t *testing.T) (serverConn, clientConn *websocket.Conn) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	connCh := make(chan *websocket.Conn, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connCh <- c
	}))
	t.Cleanup(ts.Close)

	wsURL := "ws" + ts.URL[len("http"):]
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	t.Cleanup(func() { clientConn.Close() })

	select {
	case serverConn = <-connCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted the test connection")
	}
	t.Cleanup(func() { serverConn.Close() })
	return serverConn, clientConn
}

// readControl reads and parses the next text control message from conn,
// failing the test if none arrives within the deadline or it isn't valid
// JSON — every assertion below is really asking "did the client see the
// state transitions it needs to render, in order."
func readControl(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("readControl: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("readControl: not JSON: %v (%q)", err, data)
	}
	return msg
}

// stubDialTTS lets run()-driven tests that don't care about TTS at all
// (prewarmTTS now fires on every run(), see pipeline.go) supply
// something non-nil — a nil dialTTS field would panic on the first call,
// same as a nil dialSTT would.
func stubDialTTS(string, string, moshiclient.TTSVoice, float64, time.Duration) (ttsConn, error) {
	return nil, errors.New("stub: no TTS in this test")
}

func TestRunSendsListeningAfterSuccessfulDial(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	s := &session{
		client:  serverConn,
		dialTTS: stubDialTTS,
		dialSTT: (&dialScript{fn: func(int) (sttConn, error) {
			return newFakeSTTConn(), nil
		}}).dial,
	}
	go s.run()

	if got := readControl(t, clientConn)["value"]; got != "connecting to speech recognition (cold start can take up to a minute)" {
		t.Fatalf("first control message = %v, want the connecting-state message", got)
	}
	// This is the exact regression from 2026-08-25: the connecting
	// message was sent but nothing ever cleared it, so a real cold
	// start read as permanently stuck. A successful dial must be
	// followed by "listening", not silence.
	if got := readControl(t, clientConn)["value"]; got != "listening" {
		t.Fatalf("second control message = %v, want %q", got, "listening")
	}
}

func TestRunRetriesOnDialFailureThenSucceeds(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	s := &session{
		client:  serverConn,
		dialTTS: stubDialTTS,
		dialSTT: (&dialScript{fn: func(call int) (sttConn, error) {
			if call == 0 {
				return nil, errors.New("dial failed")
			}
			return newFakeSTTConn(), nil
		}}).dial,
	}
	go s.run()

	want := []string{
		"connecting to speech recognition (cold start can take up to a minute)",
		"", // error message, checked separately below (different field)
		"connecting to speech recognition (cold start can take up to a minute)",
		"listening",
	}
	for i, w := range want {
		msg := readControl(t, clientConn)
		if i == 1 {
			if msg["type"] != "error" {
				t.Fatalf("message %d type = %v, want %q", i, msg["type"], "error")
			}
			continue
		}
		if got := msg["value"]; got != w {
			t.Fatalf("message %d value = %v, want %q", i, got, w)
		}
	}
}

func TestRunReconnectsOnSTTLoopError(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	script := &dialScript{fn: func(int) (sttConn, error) {
		// Pre-closed: Recv() returns an error immediately, as if the
		// connection had already dropped — forces runSTTLoop to
		// return right away every time, so the reconnect path fires
		// repeatedly without a real network flake.
		c := newFakeSTTConn()
		c.Close()
		return c, nil
	}}
	s := &session{client: serverConn, dialTTS: stubDialTTS, dialSTT: script.dial}
	go s.run()

	readControl(t, clientConn) // connecting (1st dial)
	readControl(t, clientConn) // listening (1st dial "succeeded")
	if got := readControl(t, clientConn)["value"]; got != "reconnecting" {
		t.Fatalf("after STT loop error, control message = %v, want %q", got, "reconnecting")
	}
	if got := readControl(t, clientConn)["value"]; got != "connecting to speech recognition (cold start can take up to a minute)" {
		// Proves run() actually redialed rather than giving up — closing
		// the client below before this point would race the redial
		// against sleepOrClientGone's own clientDone check and could
		// make run() give up having never redialed at all.
		t.Fatalf("control message after reconnecting = %v, want the connecting-state message (a redial)", got)
	}
	clientConn.Close()

	if script.callCount() < 2 {
		t.Errorf("dialSTT called %d times, want at least 2 (initial + one reconnect)", script.callCount())
	}
}

func TestRunStopsReconnectingWhenClientGone(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	script := &dialScript{fn: func(int) (sttConn, error) {
		return nil, errors.New("moshi-server unreachable")
	}}
	s := &session{client: serverConn, dialTTS: stubDialTTS, dialSTT: script.dial}

	runDone := make(chan error, 1)
	go func() { runDone <- s.run() }()

	readControl(t, clientConn) // connecting
	readControl(t, clientConn) // error
	clientConn.Close()

	select {
	case <-runDone:
		// run() returned instead of continuing to back off/retry
		// forever with nobody listening — the case
		// sleepOrClientGone's clientDone check exists for.
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return promptly after the client disconnected")
	}
}

func TestBeginGenerationStaleness(t *testing.T) {
	s := &session{}
	_, firstCancelled := s.beginGeneration()
	if firstCancelled() {
		t.Fatal("a generation should not be cancelled immediately after it begins")
	}
	_, secondCancelled := s.beginGeneration()
	if !firstCancelled() {
		t.Error("first generation's cancelled() should flip true once a second generation begins")
	}
	if secondCancelled() {
		t.Error("second (current) generation's cancelled() should stay false")
	}
}

// fakeTTSConn records SendText calls and, optionally, emits one scripted
// audio frame from Recv once SendText has been called firstFrameAfter
// times — used to prove speakStream's send and drain sides run
// concurrently (see TestSpeakStreamSendsChunksWhileDraining), not just to
// assert final call counts. Recv otherwise blocks until Close, mirroring
// fakeSTTConn's pattern above.
type fakeTTSConn struct {
	closed    atomic.Bool
	closeCh   chan struct{}
	closeOnce sync.Once

	mu              sync.Mutex
	sendTextCalls   []string
	eosCalled       bool
	firstFrameAfter int // emit one audio frame once len(sendTextCalls) reaches this; 0 = never
	frameSent       bool
	frameCh         chan struct{}
}

func newFakeTTSConn() *fakeTTSConn {
	return &fakeTTSConn{
		closeCh: make(chan struct{}),
		frameCh: make(chan struct{}, 1),
	}
}

func (f *fakeTTSConn) SendText(text string) error {
	f.mu.Lock()
	f.sendTextCalls = append(f.sendTextCalls, text)
	n, threshold := len(f.sendTextCalls), f.firstFrameAfter
	f.mu.Unlock()
	if threshold > 0 && n == threshold {
		select {
		case f.frameCh <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *fakeTTSConn) SendEOS() error {
	f.mu.Lock()
	f.eosCalled = true
	f.mu.Unlock()
	return nil
}

func (f *fakeTTSConn) Recv() (any, error) {
	f.mu.Lock()
	threshold, alreadySent := f.firstFrameAfter, f.frameSent
	f.mu.Unlock()
	if threshold > 0 && !alreadySent {
		select {
		case <-f.frameCh:
			f.mu.Lock()
			f.frameSent = true
			f.mu.Unlock()
			return moshiclient.TTSAudioMessage{PCM: []float64{0.1, 0.2}}, nil
		case <-f.closeCh:
			return nil, errors.New("fake tts: closed")
		}
	}
	<-f.closeCh
	return nil, errors.New("fake tts: closed")
}

func (f *fakeTTSConn) Close() error {
	f.closed.Store(true)
	f.closeOnce.Do(func() { close(f.closeCh) })
	return nil
}

func (f *fakeTTSConn) SendTextCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sendTextCalls...)
}

func TestCancelActiveClosesTTSAndNotifiesClient(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	tts := newFakeTTSConn()
	s := &session{client: serverConn, activeTTS: tts}

	s.cancelActive()

	if !tts.closed.Load() {
		t.Error("cancelActive did not close the active TTS connection")
	}
	if s.activeTTS != nil {
		t.Error("cancelActive did not clear activeTTS")
	}
	if got := readControl(t, clientConn)["type"]; got != "interrupted" {
		t.Fatalf("control message type = %v, want %q", got, "interrupted")
	}
}

// TestBeginGenerationDoesNotCloseActiveTTS is the corrected regression
// test for the live 2026-08-25 bug — see beginGeneration's doc comment
// for the full trace. An earlier fix made beginGeneration close the
// previous generation's activeTTS immediately on every new call's start,
// which fixed the original leak but introduced a worse one: with
// streaming, a call's TTS dial begins concurrently with its LLM call, and
// turn-taking's own speculative→real promotion fires a new call within
// about a second of the last — reliably faster than a TTS dial can
// produce a single frame. That closed replies that hadn't spoken a word
// yet, on ordinary sentences, not just fast speech (confirmed live: a
// single normally-spoken utterance produced zero audio across three
// internal calls). beginGeneration must only bump the counter now — the
// actual close-and-replace happens in speakStream, atomically, at the
// moment a new call is ready to speak (see
// TestSpeakStreamClaimsSlotAndClosesPreviousConnection).
func TestBeginGenerationDoesNotCloseActiveTTS(t *testing.T) {
	tts := newFakeTTSConn()
	s := &session{activeTTS: tts}

	s.beginGeneration()

	if tts.closed.Load() {
		t.Error("beginGeneration closed the active TTS connection — that's the 2026-08-25 regression this test guards against; the close belongs in speakStream's claim step, not here")
	}
	if s.activeTTS != tts {
		t.Error("beginGeneration touched activeTTS — it should leave whatever's currently speaking alone")
	}
}

func TestBeginGenerationDoesNotSendInterruptedMessage(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	s := &session{client: serverConn, activeTTS: newFakeTTSConn()}

	s.beginGeneration()

	// beginGeneration closes the previous TTS connection (see the test
	// above) but must not tell the client "interrupted" — that message
	// specifically means the user talked over Saturday, which starting a
	// new reply attempt on its own doesn't. cancelActive still sends it
	// (TestCancelActiveClosesTTSAndNotifiesClient above already covers
	// that). Confirm nothing arrives on this connection at all.
	clientConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := clientConn.ReadMessage(); err == nil {
		t.Error("beginGeneration sent a control message to the client; it shouldn't send anything")
	}
}

func newSessionWithFakeTTS(serverConn *websocket.Conn, tts *fakeTTSConn) *session {
	return &session{
		client: serverConn,
		dialTTS: func(string, string, moshiclient.TTSVoice, float64, time.Duration) (ttsConn, error) {
			return tts, nil
		},
	}
}

func TestSpeakStreamSendsTextPerChunkThenOneEOS(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	tts := newFakeTTSConn()
	s := newSessionWithFakeTTS(serverConn, tts)

	chunks := make(chan string, 3)
	chunks <- "hello "
	chunks <- "there, "
	chunks <- "world"
	close(chunks)

	done := make(chan error, 1)
	go func() { done <- s.speakStream(chunks, func() bool { return false }) }()

	readControl(t, clientConn) // warming up
	readControl(t, clientConn) // speaking
	tts.Close()                // end the fake audio stream so speakStream returns

	if err := <-done; err != nil {
		t.Fatalf("speakStream: %v", err)
	}

	want := []string{"hello ", "there, ", "world"}
	got := tts.SendTextCalls()
	if len(got) != len(want) {
		t.Fatalf("SendText calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SendText call %d = %q, want %q", i, got[i], want[i])
		}
	}
	tts.mu.Lock()
	eosCalled := tts.eosCalled
	tts.mu.Unlock()
	if !eosCalled {
		t.Error("SendEOS was never called")
	}
}

// TestSpeakStreamSendsChunksWhileDraining proves speakStream's send and
// drain sides run concurrently, not send-then-drain serialized — the
// actual claim the whole streaming feature rests on. The chunks channel
// is unbuffered and the test withholds the second/third chunk until it
// has already received an audio frame produced after the first chunk's
// SendText: a serialized implementation would deadlock right here, since
// drainTTSAudio would never start until every chunk had been sent, and
// the test won't send the remaining chunks until it sees this frame.
func TestSpeakStreamSendsChunksWhileDraining(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	tts := newFakeTTSConn()
	tts.firstFrameAfter = 1
	s := newSessionWithFakeTTS(serverConn, tts)

	chunks := make(chan string)
	done := make(chan error, 1)
	go func() { done <- s.speakStream(chunks, func() bool { return false }) }()

	readControl(t, clientConn) // warming up
	chunks <- "hello "
	readControl(t, clientConn) // speaking

	clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, _, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("reading the first audio frame (would time out under a serialized send-then-drain implementation): %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want BinaryMessage", mt)
	}

	chunks <- "there, "
	chunks <- "world"
	close(chunks)
	tts.Close()

	if err := <-done; err != nil {
		t.Fatalf("speakStream: %v", err)
	}
	want := []string{"hello ", "there, ", "world"}
	got := tts.SendTextCalls()
	if len(got) != len(want) {
		t.Fatalf("SendText calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SendText call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSpeakEmptyTextNeverDialsTTS(t *testing.T) {
	serverConn, _ := newWSPair(t)
	dialed := false
	s := &session{
		client: serverConn,
		dialTTS: func(string, string, moshiclient.TTSVoice, float64, time.Duration) (ttsConn, error) {
			dialed = true
			return nil, errors.New("should never be called")
		},
	}
	if err := s.speak("   "); err != nil {
		t.Fatalf("speak(whitespace-only) = %v, want nil", err)
	}
	if dialed {
		t.Error("speak(\"\") dialed TTS — the empty-text guard regressed")
	}
}

func TestPrewarmTTSMakesConnectionAvailableToClaim(t *testing.T) {
	warm := newFakeTTSConn()
	s := &session{
		clientDone: make(chan struct{}),
		dialTTS: func(string, string, moshiclient.TTSVoice, float64, time.Duration) (ttsConn, error) {
			return warm, nil
		},
	}
	s.prewarmTTS()

	got := s.claimWarmTTS()
	if got != warm {
		t.Fatalf("claimWarmTTS() = %v, want the pre-warmed connection", got)
	}
	if got := s.claimWarmTTS(); got != nil {
		t.Errorf("second claimWarmTTS() = %v, want nil — already claimed once", got)
	}
	if warm.closed.Load() {
		t.Error("a claimed connection should not have been closed by prewarmTTS/claimWarmTTS")
	}
}

func TestPrewarmTTSClosesConnectionIfClientAlreadyGone(t *testing.T) {
	clientDone := make(chan struct{})
	close(clientDone) // client gone before the (slow, cold) dial even finishes
	warm := newFakeTTSConn()
	s := &session{
		clientDone: clientDone,
		dialTTS: func(string, string, moshiclient.TTSVoice, float64, time.Duration) (ttsConn, error) {
			return warm, nil
		},
	}
	s.prewarmTTS()

	if got := s.claimWarmTTS(); got != nil {
		t.Errorf("claimWarmTTS() = %v, want nil — nobody should claim a connection dialed after the client left", got)
	}
	if !warm.closed.Load() {
		t.Error("prewarmTTS did not close the connection after finding the client already gone")
	}
}

func TestSpeakStreamUsesPrewarmedConnectionWithoutDialingFresh(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	warm := newFakeTTSConn()
	s := &session{
		client:  serverConn,
		warmTTS: warm,
		dialTTS: func(string, string, moshiclient.TTSVoice, float64, time.Duration) (ttsConn, error) {
			t.Fatal("speakStream dialed fresh instead of using the pre-warmed connection")
			return nil, nil
		},
	}

	chunks := make(chan string, 1)
	chunks <- "hi"
	close(chunks)
	done := make(chan error, 1)
	go func() { done <- s.speakStream(chunks, func() bool { return false }) }()

	// No "warming up voice" message on the pre-warmed path — there was no
	// wait to warn about, so the client should go straight to "speaking".
	if got := readControl(t, clientConn)["value"]; got != "speaking" {
		t.Fatalf("first control message = %v, want %q", got, "speaking")
	}
	warm.Close()
	if err := <-done; err != nil {
		t.Fatalf("speakStream: %v", err)
	}
	if got := warm.SendTextCalls(); len(got) != 1 || got[0] != "hi" {
		t.Errorf("SendText calls = %v, want [\"hi\"]", got)
	}
}

// TestSpeakStreamClaimsSlotAndClosesPreviousConnection is the direct
// regression test for the 2026-08-25 evidence: a call that's ready to
// speak (has a live TTS connection, and isn't cancelled) must evict
// whatever connection was previously active, exactly as the old
// beginGeneration-based close used to — just relocated to the point a
// replacement is actually ready, not the point a sibling call merely
// started. Without this, the original leak (2nd live failure this
// session) would return: an abandoned connection never gets closed.
func TestSpeakStreamClaimsSlotAndClosesPreviousConnection(t *testing.T) {
	serverConn, _ := newWSPair(t)
	previous := newFakeTTSConn()
	next := newFakeTTSConn()
	s := &session{
		client:    serverConn,
		activeTTS: previous,
		dialTTS: func(string, string, moshiclient.TTSVoice, float64, time.Duration) (ttsConn, error) {
			return next, nil
		},
	}

	chunks := make(chan string, 1)
	chunks <- "hi"
	close(chunks)
	done := make(chan error, 1)
	go func() { done <- s.speakStream(chunks, func() bool { return false }) }()

	next.Close() // end the fake audio stream so speakStream returns
	if err := <-done; err != nil {
		t.Fatalf("speakStream: %v", err)
	}

	if !previous.closed.Load() {
		t.Error("speakStream did not close the previously active TTS connection when claiming the slot")
	}
}

// TestSpeakStreamSkipsClaimWhenAlreadyCancelled proves the other half of
// the same fix: a call that turns out to be superseded by the time it has
// a connection in hand must NOT claim the slot at all — it should close
// its own (otherwise-unused) connection, drain chunks so the producer
// goroutine isn't left blocked, and leave whatever's currently active
// (a sibling reply that's actually still current) untouched.
func TestSpeakStreamSkipsClaimWhenAlreadyCancelled(t *testing.T) {
	serverConn, _ := newWSPair(t)
	active := newFakeTTSConn()
	skipped := newFakeTTSConn()
	s := &session{
		client:    serverConn,
		activeTTS: active,
		dialTTS: func(string, string, moshiclient.TTSVoice, float64, time.Duration) (ttsConn, error) {
			return skipped, nil
		},
	}

	chunks := make(chan string, 1)
	chunks <- "hi"
	close(chunks)
	err := s.speakStream(chunks, func() bool { return true })
	if err != nil {
		t.Fatalf("speakStream: %v, want nil (a superseded call isn't an error)", err)
	}

	if !skipped.closed.Load() {
		t.Error("speakStream did not close its own connection after finding itself already cancelled")
	}
	if s.activeTTS != active {
		t.Error("speakStream touched activeTTS for a call that was already cancelled — it should leave the actually-active reply alone")
	}
	if active.closed.Load() {
		t.Error("speakStream closed the still-current reply's connection — only the cancelled call's own connection should have been closed")
	}
}
