package moshiclient

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

// STTClient is a WebSocket connection to moshi-server's STT service. Not
// safe for concurrent Send* calls from multiple goroutines without
// external synchronization — mirrors gorilla/websocket's own single-writer
// requirement.
type STTClient struct {
	conn   *websocket.Conn
	mu     sync.Mutex // guards writes
	closed bool
}

// DialSTT connects to baseURL+SpeechToTextPath, sends the required
// kyutai-api-key header, and waits for the server's handshake — a Ready
// message on success, or an error (either a decode failure or an
// STTErrorMessage, wrapped as an error) if the server has no capacity.
func DialSTT(baseURL, apiKey string, timeout time.Duration) (*STTClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("moshiclient: parse STT base URL: %w", err)
	}
	u.Path = u.Path + SpeechToTextPath

	dialer := websocket.Dialer{HandshakeTimeout: timeout}
	headers := http.Header{}
	if apiKey != "" {
		headers.Set("kyutai-api-key", apiKey)
	}
	conn, _, err := dialer.Dial(u.String(), headers)
	if err != nil {
		return nil, fmt.Errorf("moshiclient: dial STT %s: %w", u.String(), err)
	}

	c := &STTClient{conn: conn}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("moshiclient: STT handshake read: %w", err)
	}
	msg, err := decodeSTTMessage(raw)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("moshiclient: STT handshake decode: %w", err)
	}
	switch m := msg.(type) {
	case STTReadyMessage:
		return c, nil
	case STTErrorMessage:
		conn.Close()
		return nil, fmt.Errorf("moshiclient: STT server error: %s", m.Message)
	default:
		conn.Close()
		return nil, fmt.Errorf("moshiclient: STT handshake: expected Ready or Error, got %T", msg)
	}
}

// SendAudio sends one frame of mono float32 PCM. Callers should send
// SamplesPerFrame samples at a time to match moshi-server's expected
// framing, though it doesn't strictly enforce this.
func (c *STTClient) SendAudio(pcm []float32) error {
	return c.send(sttAudioMsg{Type: "Audio", PCM: pcm})
}

// SendMarker sends an echo-back marker.
func (c *STTClient) SendMarker(id int) error {
	return c.send(sttMarkerMsg{Type: "Marker", ID: id})
}

func (c *STTClient) send(v any) error {
	b, err := msgpack.Marshal(v)
	if err != nil {
		return fmt.Errorf("moshiclient: encode STT message: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("moshiclient: STT connection closed")
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, b)
}

// Recv blocks until the next STT message arrives and returns it as one of
// STTWordMessage, STTEndWordMessage, STTMarkerMessage, STTStepMessage, or
// STTErrorMessage. Returns an error on a closed/broken connection or a
// malformed message.
func (c *STTClient) Recv() (any, error) {
	_, raw, err := c.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	return decodeSTTMessage(raw)
}

// Close closes the underlying WebSocket connection. Safe to call more
// than once.
func (c *STTClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

// decodeSTTMessage sniffs the "type" discriminator and decodes raw
// msgpack bytes into the matching STT*Message type. Factored out from
// Recv so it's testable against recorded fixture bytes with no live
// connection.
func decodeSTTMessage(raw []byte) (any, error) {
	var peek struct {
		Type string `msgpack:"type"`
	}
	if err := msgpack.Unmarshal(raw, &peek); err != nil {
		return nil, fmt.Errorf("moshiclient: decode STT message type: %w", err)
	}
	switch peek.Type {
	case "Word":
		var m STTWordMessage
		return m, msgpack.Unmarshal(raw, &m)
	case "EndWord":
		var m STTEndWordMessage
		return m, msgpack.Unmarshal(raw, &m)
	case "Marker":
		var m STTMarkerMessage
		return m, msgpack.Unmarshal(raw, &m)
	case "Step":
		var m STTStepMessage
		return m, msgpack.Unmarshal(raw, &m)
	case "Error":
		var m STTErrorMessage
		return m, msgpack.Unmarshal(raw, &m)
	case "Ready":
		var m STTReadyMessage
		return m, msgpack.Unmarshal(raw, &m)
	default:
		return nil, fmt.Errorf("moshiclient: unknown STT message type %q", peek.Type)
	}
}
