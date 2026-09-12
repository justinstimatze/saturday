package main

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"saturday/moshiclient"
)

// ackPhrases are the fixed candidate acks — short, content-free (nothing
// here could read as answering a question the user didn't ask), spoken
// the instant STT's turn-taking model considers a pause likely, before
// the real reply's routing/expansion has even run. Modeled on retired
// saturday-audio's V0.2.2 STOCK_PHRASES list, not ported literally.
var ackPhrases = []string{
	"On it.",
	"Got it.",
	"Right away.",
	"Sure thing.",
	"One sec.",
}

// ackPool holds ackPhrases pre-synthesized once, via a one-shot
// moshiclient TTS call each, at process startup — see newAckPool. Playing
// one back later is just sendAudio: no synthesis, no network call, no
// orchestrator/session dependency. Nil-safe: a nil *ackPool (synthesis
// failed at startup) makes random() always return nil, and callers treat
// that as "acks disabled" — never a required feature. One *ackPool is
// shared across every connected session (see main.go), so random() guards
// the *rand.Rand with a mutex — rand.Rand from rand.New is not itself
// safe for concurrent use.
type ackPool struct {
	mu    sync.Mutex
	clips [][]float64
	rng   *rand.Rand
}

// newAckPool synthesizes every ackPhrases entry via a standalone
// Dial->SendText->SendEOS->drain Recv()->Close round trip (moshiclient's
// TTS protocol has no session/streaming coupling — confirmed against
// moshiclient/tts.go). Returns an error if any phrase fails; callers
// should log and pass a nil *ackPool through rather than block server
// startup on it — acks are a latency nicety, not load-bearing.
func newAckPool(ttsURL, apiKey string, voice moshiclient.TTSVoice, dialTimeout time.Duration) (*ackPool, error) {
	p := &ackPool{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
	for _, phrase := range ackPhrases {
		pcm, err := synthOnce(ttsURL, apiKey, voice, phrase, dialTimeout)
		if err != nil {
			return nil, fmt.Errorf("synthesize ack %q: %w", phrase, err)
		}
		p.clips = append(p.clips, pcm)
	}
	return p, nil
}

func synthOnce(ttsURL, apiKey string, voice moshiclient.TTSVoice, phrase string, dialTimeout time.Duration) ([]float64, error) {
	tts, err := moshiclient.DialTTS(ttsURL, apiKey, voice, 1.5, dialTimeout)
	if err != nil {
		return nil, err
	}
	defer tts.Close()
	if err := tts.SendText(phrase); err != nil {
		return nil, err
	}
	if err := tts.SendEOS(); err != nil {
		return nil, err
	}
	var pcm []float64
	for {
		msg, err := tts.Recv()
		if err != nil {
			// Server closed the stream — same "not an error" contract as
			// drainTTSAudio: a closed connection after EOS is the normal
			// end of a synthesis turn.
			return pcm, nil
		}
		switch m := msg.(type) {
		case moshiclient.TTSAudioMessage:
			pcm = append(pcm, m.PCM...)
		case moshiclient.TTSErrorMessage:
			return nil, fmt.Errorf("tts error: %s", m.Message)
		}
	}
}

// random returns one cached ack clip, or nil if the pool is nil/empty.
func (p *ackPool) random() []float64 {
	if p == nil || len(p.clips) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clips[p.rng.Intn(len(p.clips))]
}
