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

func TestRunSendsListeningAfterSuccessfulDial(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	s := &session{
		client: serverConn,
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
		client: serverConn,
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
	s := &session{client: serverConn, dialSTT: script.dial}
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
	s := &session{client: serverConn, dialSTT: script.dial}

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

type fakeTTSConn struct {
	closed atomic.Bool
}

func (f *fakeTTSConn) SendText(string) error { return nil }
func (f *fakeTTSConn) SendEOS() error        { return nil }
func (f *fakeTTSConn) Recv() (any, error)    { return nil, errors.New("fake tts: not scripted") }
func (f *fakeTTSConn) Close() error          { f.closed.Store(true); return nil }

func TestCancelActiveClosesTTSAndNotifiesClient(t *testing.T) {
	serverConn, clientConn := newWSPair(t)
	tts := &fakeTTSConn{}
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
