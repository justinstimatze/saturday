package moshiclient

import (
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

func TestDecodeSTTMessage(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want func(t *testing.T, got any)
	}{
		{
			"Word",
			map[string]any{"type": "Word", "text": "hello", "start_time": 1.5},
			func(t *testing.T, got any) {
				m, ok := got.(STTWordMessage)
				if !ok || m.Text != "hello" || m.StartTime != 1.5 {
					t.Errorf("got %#v", got)
				}
			},
		},
		{
			"Step",
			map[string]any{"type": "Step", "step_idx": 42, "prs": []float64{0.1, 0.2, 0.73}},
			func(t *testing.T, got any) {
				m, ok := got.(STTStepMessage)
				if !ok || m.StepIdx != 42 || len(m.Prs) != 3 || m.Prs[2] != 0.73 {
					t.Errorf("got %#v", got)
				}
			},
		},
		{
			"Ready",
			map[string]any{"type": "Ready"},
			func(t *testing.T, got any) {
				if _, ok := got.(STTReadyMessage); !ok {
					t.Errorf("got %#v, want STTReadyMessage", got)
				}
			},
		},
		{
			"Error",
			map[string]any{"type": "Error", "message": "no capacity"},
			func(t *testing.T, got any) {
				m, ok := got.(STTErrorMessage)
				if !ok || m.Message != "no capacity" {
					t.Errorf("got %#v", got)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := msgpack.Marshal(c.in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeSTTMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			c.want(t, got)
		})
	}
}

func TestDecodeSTTMessageUnknownType(t *testing.T) {
	raw, _ := msgpack.Marshal(map[string]any{"type": "SomethingNew"})
	if _, err := decodeSTTMessage(raw); err == nil {
		t.Error("expected an error for an unknown message type")
	}
}

func TestDecodeTTSMessage(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want func(t *testing.T, got any)
	}{
		{
			"Audio",
			map[string]any{"type": "Audio", "pcm": []float64{0.1, -0.2, 0.3}},
			func(t *testing.T, got any) {
				m, ok := got.(TTSAudioMessage)
				if !ok || len(m.PCM) != 3 {
					t.Errorf("got %#v", got)
				}
			},
		},
		{
			"Text",
			map[string]any{"type": "Text", "text": "hi", "start_s": 0.5, "stop_s": 1.0},
			func(t *testing.T, got any) {
				m, ok := got.(TTSTextMessage)
				if !ok || m.Text != "hi" {
					t.Errorf("got %#v", got)
				}
			},
		},
		{
			"Ready",
			map[string]any{"type": "Ready"},
			func(t *testing.T, got any) {
				if _, ok := got.(TTSReadyMessage); !ok {
					t.Errorf("got %#v, want TTSReadyMessage", got)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := msgpack.Marshal(c.in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeTTSMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			c.want(t, got)
		})
	}
}

func TestClientMessageEncodeRoundTrip(t *testing.T) {
	// sttAudioMsg / ttsTextMsg / ttsEosMsg are what we SEND — verify they
	// encode with the field names moshi-server expects, by decoding back
	// into a generic map and checking the keys.
	raw, err := msgpack.Marshal(sttAudioMsg{Type: "Audio", PCM: []float32{0.1, 0.2}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := msgpack.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "Audio" {
		t.Errorf("type = %v, want Audio", m["type"])
	}
	if _, ok := m["pcm"]; !ok {
		t.Error("expected a \"pcm\" key")
	}

	raw, err = msgpack.Marshal(ttsTextMsg{Type: "Text", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	m = nil
	if err := msgpack.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "Text" || m["text"] != "hello" {
		t.Errorf("got %#v", m)
	}
}
