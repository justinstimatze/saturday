package orchestrator

import "testing"

func TestWordBatcherWordSplitAcrossFeedCalls(t *testing.T) {
	var b wordBatcher
	if flushed, ok := b.Feed("hel"); ok {
		t.Fatalf("Feed(%q) = (%q, true), want (_, false) — nothing to flush mid-word", "hel", flushed)
	}
	// Short and no sentence-ending punctuation: held below minFlushChars.
	if flushed, ok := b.Feed("lo world "); ok {
		t.Fatalf("Feed(...) = (%q, true), want ok=false — below minFlushChars with no sentence end", flushed)
	}
	flushed, ok := b.Feed("this sentence is long enough now. ")
	if !ok {
		t.Fatal("Feed(...) = (_, false), want ok=true once minFlushChars is reached")
	}
	if want := "hello world this sentence is long enough now. "; flushed != want {
		t.Errorf("flushed = %q, want %q", flushed, want)
	}
}

func TestWordBatcherFlushesImmediatelyOnSentenceEnd(t *testing.T) {
	var b wordBatcher
	flushed, ok := b.Feed("Hi. ")
	if !ok {
		t.Fatal("expected ok=true — sentence-terminal punctuation flushes regardless of length")
	}
	if want := "Hi. "; flushed != want {
		t.Errorf("flushed = %q, want %q", flushed, want)
	}
}

func TestWordBatcherHoldsShortSpanBelowThreshold(t *testing.T) {
	var b wordBatcher
	if flushed, ok := b.Feed("one two three "); ok {
		t.Fatalf("Feed(...) = (%q, true), want ok=false — 14 chars, no sentence end, below minFlushChars", flushed)
	}
	flushed, ok := b.Feed("four five six seven ")
	if !ok {
		t.Fatal("expected ok=true once accumulated span crosses minFlushChars")
	}
	if want := "one two three four five six seven "; flushed != want {
		t.Errorf("flushed = %q, want %q", flushed, want)
	}
}

func TestWordBatcherTrailingWordNeedsExplicitFlush(t *testing.T) {
	var b wordBatcher
	if flushed, ok := b.Feed("done"); ok {
		t.Fatalf("Feed(%q) = (%q, true), want ok=false — no trailing whitespace yet", "done", flushed)
	}
	if got := b.Flush(); got != "done" {
		t.Errorf("Flush() = %q, want %q", got, "done")
	}
	// A second Flush with nothing new fed should be empty, not repeat.
	if got := b.Flush(); got != "" {
		t.Errorf("second Flush() = %q, want empty", got)
	}
}

func TestWordBatcherRetainsPartialWordAfterFlush(t *testing.T) {
	var b wordBatcher
	if _, ok := b.Feed("hi there, par"); ok {
		t.Fatal("expected ok=false — 'hi there, ' is short with no sentence end, held below minFlushChars")
	}
	// "par" (the trailing fragment) should still be buffered — feeding
	// the rest of the word and a final Flush should recover it whole,
	// alongside the still-held-back "hi there, " prefix.
	if flushed, ok := b.Feed("tial"); ok {
		t.Fatalf("Feed(%q) = (%q, true), want ok=false — still mid-word", "tial", flushed)
	}
	if got := b.Flush(); got != "hi there, partial" {
		t.Errorf("Flush() = %q, want %q", got, "hi there, partial")
	}
}
