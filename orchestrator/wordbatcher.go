package orchestrator

import "strings"

// minFlushChars is the smallest chunk wordBatcher will hand to TTS on an
// ordinary whitespace boundary (sentence-ending punctuation still flushes
// immediately regardless of length — see Feed).
const minFlushChars = 24

// wordBatcher buffers streamed text deltas and releases them in clause-
// sized chunks — batching TTS input at natural phrase boundaries instead
// of forwarding raw, possibly mid-word LLM deltas one at a time (see
// answerAskStreaming). Pure string processing: no goroutines, channels, or
// network, so it's unit-testable on its own.
//
// The original design flushed on every single whitespace boundary, so a
// typical reply fed moshi-server one word per SendText call (live-verified
// 2026-08-25: 17 separate Text messages for a ~130-character reply). That
// produced an audible spike/distortion at the start of nearly every word
// ("like someone speaking too close to the mic") — one synthesis restart
// per word instead of continuous prosody across a phrase. Feed now holds a
// completed word back until the accumulated flushable text reaches
// minFlushChars, unless it already ends in sentence-terminal punctuation
// (.!?), which flushes immediately regardless of length so short replies
// and natural clause breaks aren't held hostage waiting to hit the
// threshold. Still arrives well before the full reply finishes generating
// for anything beyond a handful of words — just batched enough for
// moshi-server to synthesize smoothly.
type wordBatcher struct {
	buf strings.Builder
}

// Feed appends delta and, once the accumulated buffer contains a
// whitespace-terminated span that's either at least minFlushChars long or
// ends in sentence-terminal punctuation, returns that span (flushed,
// true). Trailing content after the flushed span stays buffered for the
// next Feed. Returns ("", false) if there's nothing flushable yet — either
// no whitespace boundary at all, or the flushable span is still too short
// and doesn't end a sentence.
func (b *wordBatcher) Feed(delta string) (flushed string, ok bool) {
	b.buf.WriteString(delta)
	s := b.buf.String()
	idx := strings.LastIndexAny(s, " \t\n")
	if idx < 0 {
		return "", false
	}
	candidate := s[:idx+1]
	trimmed := strings.TrimRight(candidate, " \t\n")
	sentenceEnd := trimmed != "" && strings.ContainsRune(".!?", rune(trimmed[len(trimmed)-1]))
	if len(trimmed) < minFlushChars && !sentenceEnd {
		return "", false
	}
	flushed = candidate
	b.buf.Reset()
	b.buf.WriteString(s[idx+1:])
	return flushed, true
}

// Flush returns and clears whatever's left buffered — the final word (or
// fragment), which has no trailing whitespace to trigger Feed's own
// flush. Call once after the source of deltas is exhausted, so the last
// word isn't silently dropped.
func (b *wordBatcher) Flush() string {
	s := b.buf.String()
	b.buf.Reset()
	return s
}
