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

// TTSVoice is a moshi-server voice identifier, e.g.
// "unmute-prod-website/ex04_narration_longform_00001.wav".
type TTSVoice string

// TTSClient is a WebSocket connection to moshi-server's TTS service. Not
// safe for concurrent Send* calls from multiple goroutines without
// external synchronization.
type TTSClient struct {
	conn   *websocket.Conn
	mu     sync.Mutex // guards writes
	closed bool
}

// DialTTS connects to baseURL+TextToSpeechPath with the given voice and
// cfgAlpha (Unmute's own default is 1.5; passing 0 omits the parameter
// and lets moshi-server use its own default), sends the required
// kyutai-api-key header, and waits for the handshake. Retries up to 10
// times on an unexpected-but-not-Error message before giving up — ported
// from Unmute's own client, which notes stray packets from a previous
// client's session can arrive first due to a server-side race.
func DialTTS(baseURL, apiKey string, voice TTSVoice, cfgAlpha float64, timeout time.Duration) (*TTSClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("moshiclient: parse TTS base URL: %w", err)
	}
	wsScheme(u)
	u.Path = u.Path + TextToSpeechPath
	q := u.Query()
	q.Set("format", "PcmMessagePack")
	if voice != "" {
		q.Set("voice", string(voice))
	}
	if cfgAlpha != 0 {
		q.Set("cfg_alpha", fmt.Sprintf("%g", cfgAlpha))
	}
	u.RawQuery = q.Encode()

	dialer := websocket.Dialer{HandshakeTimeout: timeout}
	headers := http.Header{}
	if apiKey != "" {
		headers.Set("kyutai-api-key", apiKey)
	}
	conn, _, err := dialer.Dial(u.String(), headers)
	if err != nil {
		return nil, fmt.Errorf("moshiclient: dial TTS %s: %w", u.String(), err)
	}

	c := &TTSClient{conn: conn}
	const maxHandshakeAttempts = 10
	for i := 0; i < maxHandshakeAttempts; i++ {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("moshiclient: TTS handshake read: %w", err)
		}
		msg, err := decodeTTSMessage(raw)
		if err != nil {
			// Unexpected/stray packet — matches Unmute's own tolerance for
			// a race where a previous session's packets arrive first.
			continue
		}
		switch m := msg.(type) {
		case TTSReadyMessage:
			return c, nil
		case TTSErrorMessage:
			conn.Close()
			return nil, fmt.Errorf("moshiclient: TTS server error: %s", m.Message)
		default:
			continue
		}
	}
	conn.Close()
	return nil, fmt.Errorf("moshiclient: TTS handshake: no Ready after %d attempts", maxHandshakeAttempts)
}

// SendText asks the server to synthesize text. Empty strings are silently
// dropped — mirrors Unmute's own client, which never sends an empty Text
// message.
func (c *TTSClient) SendText(text string) error {
	if text == "" {
		return nil
	}
	return c.send(ttsTextMsg{Type: "Text", Text: text})
}

// SendEOS tells the server no more text is coming for this turn.
func (c *TTSClient) SendEOS() error {
	return c.send(ttsEosMsg{Type: "Eos"})
}

func (c *TTSClient) send(v any) error {
	b, err := msgpack.Marshal(v)
	if err != nil {
		return fmt.Errorf("moshiclient: encode TTS message: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("moshiclient: TTS connection closed")
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, b)
}

// Recv blocks until the next TTS message arrives and returns it as one of
// TTSTextMessage, TTSAudioMessage, or TTSErrorMessage. Returns an error on
// a closed/broken connection or a malformed message.
func (c *TTSClient) Recv() (any, error) {
	_, raw, err := c.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	return decodeTTSMessage(raw)
}

// Close closes the underlying WebSocket connection. Safe to call more
// than once.
func (c *TTSClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

// decodeTTSMessage sniffs the "type" discriminator and decodes raw
// msgpack bytes into the matching TTS*Message type. Factored out from
// Recv so it's testable against recorded fixture bytes with no live
// connection.
func decodeTTSMessage(raw []byte) (any, error) {
	var peek struct {
		Type string `msgpack:"type"`
	}
	if err := msgpack.Unmarshal(raw, &peek); err != nil {
		return nil, fmt.Errorf("moshiclient: decode TTS message type: %w", err)
	}
	switch peek.Type {
	case "Text":
		var m TTSTextMessage
		return m, msgpack.Unmarshal(raw, &m)
	case "Audio":
		var m TTSAudioMessage
		return m, msgpack.Unmarshal(raw, &m)
	case "Error":
		var m TTSErrorMessage
		return m, msgpack.Unmarshal(raw, &m)
	case "Ready":
		var m TTSReadyMessage
		return m, msgpack.Unmarshal(raw, &m)
	default:
		return nil, fmt.Errorf("moshiclient: unknown TTS message type %q", peek.Type)
	}
}
