package llmcore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONStringFieldDecoderEscapeSplitAcrossChunks(t *testing.T) {
	d := newJSONStringFieldDecoder("reply")
	var got strings.Builder
	got.WriteString(d.Feed(`"reply":"line one\`))
	got.WriteString(d.Feed(`nline two"`))
	if want := "line one\nline two"; got.String() != want {
		t.Errorf("got %q, want %q", got.String(), want)
	}
}

func TestJSONStringFieldDecoderUnicodeEscapeSplitMidSequence(t *testing.T) {
	d := newJSONStringFieldDecoder("reply")
	var got strings.Builder
	got.WriteString(d.Feed(`"reply":"\u00`))
	got.WriteString(d.Feed(`41"`))
	if want := "A"; got.String() != want {
		t.Errorf("got %q, want %q", got.String(), want)
	}
}

// TestJSONStringFieldDecoderSpaceAfterColon is the direct regression test
// for the 2026-08-25 live bug: the real Anthropic API streams tool-input
// JSON with a space after the key's colon (`"reply": "text`, confirmed via
// raw partial_json logging against a live call), not the zero-whitespace
// form (`"reply":"text`) every existing fixture in this file assumed. The
// original decoder's prefix match required the opening quote immediately
// after the colon, so it never matched real traffic — found never flipped
// true, onDelta was never called for the entire stream on every single
// call, and streamed TTS was silently silent end to end while the
// separately-decoded final reply text was always correct. This split
// mirrors the real SSE framing: the key+colon arrives in one delta, the
// space+opening-quote+value start in the next.
func TestJSONStringFieldDecoderSpaceAfterColon(t *testing.T) {
	d := newJSONStringFieldDecoder("reply")
	var got strings.Builder
	got.WriteString(d.Feed(`{"reply":`))
	got.WriteString(d.Feed(` "Core`))
	got.WriteString(d.Feed(`s spinning."}`))
	if want := "Cores spinning."; got.String() != want {
		t.Errorf("got %q, want %q", got.String(), want)
	}
}

// TestJSONStringFieldDecoderWhitespaceSplitAcrossChunks covers the
// narrower case within the same fix: the whitespace between the colon and
// the opening quote arriving as its own delta, with nothing else in it —
// Feed must return "" and wait, not give up, until the quote itself
// arrives in a later call.
func TestJSONStringFieldDecoderWhitespaceSplitAcrossChunks(t *testing.T) {
	d := newJSONStringFieldDecoder("reply")
	if got := d.Feed(`"reply":`); got != "" {
		t.Errorf("Feed after key+colon = %q, want \"\" (no value seen yet)", got)
	}
	if got := d.Feed(` `); got != "" {
		t.Errorf("Feed on whitespace-only delta = %q, want \"\"", got)
	}
	if got := d.Feed(`"hi"`); got != "hi" {
		t.Errorf("Feed after opening quote arrives = %q, want %q", got, "hi")
	}
}

func TestJSONStringFieldDecoderMultiByteUTF8SplitAcrossChunks(t *testing.T) {
	d := newJSONStringFieldDecoder("reply")
	full := `"reply":"café done"`
	// Split exactly inside é's two-byte UTF-8 encoding (0xC3 0xA9) — a
	// non-ASCII lead/continuation byte can never collide with the '"'/'\'
	// bytes this decoder treats specially, so no escape-aware handling is
	// needed for this to survive the split (see the type's doc comment).
	idx := strings.Index(full, "é")
	splitAt := idx + 1
	var got strings.Builder
	got.WriteString(d.Feed(full[:splitAt]))
	got.WriteString(d.Feed(full[splitAt:]))
	if want := "café done"; got.String() != want {
		t.Errorf("got %q, want %q", got.String(), want)
	}
}

func sseFrame(event string, data any) string {
	b, _ := json.Marshal(data)
	return "event: " + event + "\ndata: " + string(b) + "\n\n"
}

// buildSSEBody assembles a real-shaped Anthropic streaming response around
// a forced single tool_use block, carrying partialJSONChunks as successive
// input_json_delta events — the shape CachedCallStreaming parses.
func buildSSEBody(partialJSONChunks []string) string {
	var sb strings.Builder
	sb.WriteString(sseFrame("message_start", map[string]any{"type": "message_start"}))
	sb.WriteString(sseFrame("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "tool_use", "id": "toolu_test", "name": "answer", "input": map[string]any{}},
	}))
	for _, c := range partialJSONChunks {
		sb.WriteString(sseFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": c},
		}))
	}
	sb.WriteString(sseFrame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
	sb.WriteString(sseFrame("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}}))
	sb.WriteString(sseFrame("message_stop", map[string]any{"type": "message_stop"}))
	return sb.String()
}

// withFakeAPI points the package-level apiURL var at a local httptest
// server for the duration of the test, restoring it after — the seam
// plan-check flagged as missing for testing CachedCall/CachedCallStreaming
// without a live Anthropic API dependency.
func withFakeAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	orig := apiURL
	apiURL = ts.URL
	t.Cleanup(func() { apiURL = orig })
}

func TestCachedCallStreamingMiss(t *testing.T) {
	body := buildSSEBody([]string{`{"reply"`, `:"hello `, `there"}`})
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(body))
	})

	cacheDir := t.TempDir()
	var deltas []string
	out, err := CachedCallStreaming("test-key", "test-model", "sys", "user", Tool{Name: "answer"}, cacheDir, "cid-miss", "reply", nil, func(chunk string) {
		deltas = append(deltas, chunk)
	})
	if err != nil {
		t.Fatalf("CachedCallStreaming: %v", err)
	}
	if got := out["reply"]; got != "hello there" {
		t.Errorf("out[reply] = %v, want %q", got, "hello there")
	}
	if got := strings.Join(deltas, ""); got != "hello there" {
		t.Errorf("streamed deltas joined = %q, want %q", got, "hello there")
	}
	if len(deltas) < 2 {
		t.Errorf("expected multiple incremental deltas (not one final blob), got %d: %v", len(deltas), deltas)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cid-miss.json")); err != nil {
		t.Errorf("expected cache file to be written: %v", err)
	}
}

// TestCachedCallStreamingMissRealSSEShape mirrors the exact partial_json
// framing captured live on 2026-08-25 (see
// TestJSONStringFieldDecoderSpaceAfterColon) end to end through
// CachedCallStreaming, not just the decoder in isolation — the regression
// this file's other fixtures didn't catch precisely because they all used
// the zero-whitespace form the live API doesn't actually send.
func TestCachedCallStreamingMissRealSSEShape(t *testing.T) {
	body := buildSSEBody([]string{`{"reply":`, ` "Core`, `s spinning."}`})
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(body))
	})

	cacheDir := t.TempDir()
	var deltas []string
	out, err := CachedCallStreaming("test-key", "test-model", "sys", "user", Tool{Name: "answer"}, cacheDir, "cid-real-shape", "reply", nil, func(chunk string) {
		deltas = append(deltas, chunk)
	})
	if err != nil {
		t.Fatalf("CachedCallStreaming: %v", err)
	}
	if got := out["reply"]; got != "Cores spinning." {
		t.Errorf("out[reply] = %v, want %q", got, "Cores spinning.")
	}
	if got := strings.Join(deltas, ""); got != "Cores spinning." {
		t.Errorf("streamed deltas joined = %q, want %q — this is exactly the case that reached the client as silence", got, "Cores spinning.")
	}
	if len(deltas) == 0 {
		t.Fatal("onDelta was never called — this is the exact 2026-08-25 bug: reply text correct, zero chunks streamed to TTS")
	}
}

func TestCachedCallStreamingCacheHit(t *testing.T) {
	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, "cid-hit.json")
	if err := os.WriteFile(cachePath, []byte(`{"reply":"cached answer"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP should not be called on a cache hit")
	})

	var deltas []string
	out, err := CachedCallStreaming("test-key", "test-model", "sys", "user", Tool{Name: "answer"}, cacheDir, "cid-hit", "reply", nil, func(chunk string) {
		deltas = append(deltas, chunk)
	})
	if err != nil {
		t.Fatalf("CachedCallStreaming: %v", err)
	}
	if got := out["reply"]; got != "cached answer" {
		t.Errorf("out[reply] = %v, want %q", got, "cached answer")
	}
	if len(deltas) != 1 || deltas[0] != "cached answer" {
		t.Errorf("deltas = %v, want a single %q chunk", deltas, "cached answer")
	}
}

func TestCachedCallStreamingCancellation(t *testing.T) {
	body := buildSSEBody([]string{`{"reply"`, `:"hello `, `there, this keeps going`, ` for a while"}`})
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(body))
	})

	cacheDir := t.TempDir()
	events := 0
	cancelled := func() bool {
		events++
		return events > 3
	}
	_, err := CachedCallStreaming("test-key", "test-model", "sys", "user", Tool{Name: "answer"}, cacheDir, "cid-cancel", "reply", cancelled, func(string) {})
	if err != ErrCancelled {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "cid-cancel.json")); statErr == nil {
		t.Error("cache file should not be written on a cancelled call")
	}
}
