// Package moshiclient talks to Kyutai's moshi-server STT/TTS services
// directly over their native msgpack/WebSocket protocol — no Unmute
// backend in between. Field names and message shapes here are ported
// directly from Unmute's own client implementation
// (unmute/stt/speech_to_text.py, unmute/tts/text_to_speech.py), read from
// source rather than guessed, since this is the wire contract moshi-server
// itself expects.
package moshiclient

import "net/url"

// --- STT: client → server ---

// sttAudioMsg carries one frame of mono float32 PCM at 24kHz,
// SamplesPerFrame samples per frame (see Constants).
type sttAudioMsg struct {
	Type string    `msgpack:"type"` // "Audio"
	PCM  []float32 `msgpack:"pcm"`
}

// sttMarkerMsg is an echo-back marker the server repeats verbatim once its
// processing catches up to this point in the stream — not currently used
// by saturday-voice's turn-taking logic, exported for completeness/future
// use.
type sttMarkerMsg struct {
	Type string `msgpack:"type"` // "Marker"
	ID   int    `msgpack:"id"`
}

// --- STT: server → client ---

// STTWordMessage is a finalized transcribed word.
type STTWordMessage struct {
	Type      string  `msgpack:"type"` // "Word"
	Text      string  `msgpack:"text"`
	StartTime float64 `msgpack:"start_time"`
}

// STTEndWordMessage marks the end of the most recent word's audio span.
type STTEndWordMessage struct {
	Type     string  `msgpack:"type"` // "EndWord"
	StopTime float64 `msgpack:"stop_time"`
}

// STTMarkerMessage echoes a marker sent by the client.
type STTMarkerMessage struct {
	Type string `msgpack:"type"` // "Marker"
	ID   int    `msgpack:"id"`
}

// STTStepMessage is emitted once per 80ms audio frame. Prs[2] is Kyutai's
// own semantic pause-prediction score — no external VAD needed, see
// TurnTaking.
type STTStepMessage struct {
	Type    string    `msgpack:"type"` // "Step"
	StepIdx int       `msgpack:"step_idx"`
	Prs     []float64 `msgpack:"prs"`
}

// STTErrorMessage is returned instead of Ready on connect if the server
// has no capacity, or asynchronously on a processing error.
type STTErrorMessage struct {
	Type    string `msgpack:"type"` // "Error"
	Message string `msgpack:"message"`
}

// STTReadyMessage is the required first message on a healthy connection.
type STTReadyMessage struct {
	Type string `msgpack:"type"` // "Ready"
}

// --- TTS: client → server ---

// ttsTextMsg asks the server to synthesize text. Sent once per word/chunk
// as a reply streams in from the orchestrator.
type ttsTextMsg struct {
	Type string `msgpack:"type"` // "Text"
	Text string `msgpack:"text"`
}

// ttsEosMsg tells the server no more text is coming for this turn.
type ttsEosMsg struct {
	Type string `msgpack:"type"` // "Eos"
}

// --- TTS: server → client ---

// TTSTextMessage echoes back the text being spoken, with its audio-time
// span — useful for a client-side transcript display, not required for
// turn-taking.
type TTSTextMessage struct {
	Type   string  `msgpack:"type"` // "Text"
	Text   string  `msgpack:"text"`
	StartS float64 `msgpack:"start_s"`
	StopS  float64 `msgpack:"stop_s"`
}

// TTSAudioMessage carries one frame of synthesized mono PCM at 24kHz.
// PCM is float64, not float32: unlike Python's dynamically-typed float
// (which silently absorbs either wire precision), Go's msgpack decoder
// refuses to narrow a float64-encoded wire value into a float32 field —
// decoding as float64 accepts either precision moshi-server might use on
// the wire. Convert to float32 at the point of use if needed.
type TTSAudioMessage struct {
	Type string    `msgpack:"type"` // "Audio"
	PCM  []float64 `msgpack:"pcm"`
}

// TTSErrorMessage is returned instead of Ready on connect if the server
// has no capacity, or asynchronously on a processing error.
type TTSErrorMessage struct {
	Type    string `msgpack:"type"` // "Error"
	Message string `msgpack:"message"`
}

// TTSReadyMessage is the required first message on a healthy connection.
type TTSReadyMessage struct {
	Type string `msgpack:"type"` // "Ready"
}

// --- shared constants (ported from unmute/kyutai_constants.py) ---

const (
	// SampleRate is the PCM sample rate both STT and TTS speak, in Hz.
	SampleRate = 24000
	// SamplesPerFrame is the number of samples in one audio frame.
	SamplesPerFrame = 1920
	// FrameTimeSec is one frame's duration: SamplesPerFrame / SampleRate.
	FrameTimeSec = float64(SamplesPerFrame) / float64(SampleRate) // 0.08
	// SpeechToTextPath is moshi-server's STT WebSocket endpoint path.
	SpeechToTextPath = "/api/asr-streaming"
	// TextToSpeechPath is moshi-server's TTS WebSocket endpoint path.
	TextToSpeechPath = "/api/tts_streaming"
	// STTDelaySec is how long to pad the STT stream with silence after a
	// detected pause before treating its output as final — moshi-server's
	// STT is itself delayed relative to real time by roughly this much.
	STTDelaySec = 0.5
)

// wsScheme rewrites an http(s) baseURL to the matching ws(s) scheme gorilla/
// websocket's Dialer requires — callers (flags, docs, moshi-server's own
// Runpod/Modal proxy URLs) all use http(s), and gorilla/websocket rejects
// anything else as a "malformed ws or wss URL". Leaves ws/wss untouched so
// a caller who already got it right isn't double-converted.
func wsScheme(u *url.URL) {
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
}
