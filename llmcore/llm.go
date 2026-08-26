// Package llmcore is the shared substrate for Saturday's LLM-driven
// pipeline stages. The router (eval/router/main.go) and the expander
// (eval/expander_backtest.go + saturday-mayor/main.go) duplicated the
// Anthropic API plumbing, content-hash cache, system prompts, and tool
// schemas. This package is the single source of truth.
//
// Cache-key compatibility: the cid derivation here MUST stay byte-identical
// to the pre-lift code paths so existing .cache/*.json files keep hitting.
// If you change cacheKey or the strings passed to it (route-baseline /
// expand-baseline tags, json.Marshal vs MarshalIndent on the state arg),
// every cached LLM response in eval/.cache, eval/router/.cache, and
// saturday-mayor/.cache becomes stale and re-runs cost real tokens.
package llmcore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	APIURL     = "https://api.anthropic.com/v1/messages"
	APIVersion = "2023-06-01"
	// RouterModel — runs on every utterance; latency-sensitive, Haiku.
	RouterModel = "claude-haiku-4-5"
	// ExpanderModel — V0.2.7: upgraded to Sonnet. The hotphrase ("would you
	// kindly") is an explicit user opt-in to the expand path, so the user
	// has already accepted the latency tax; trade ms for fewer over-cautious
	// asks/declines. Phase 3 summarizer stays on Haiku via SummarizerModel.
	ExpanderModel   = "claude-sonnet-4-6"
	SummarizerModel = "claude-haiku-4-5"
)

// apiURL is APIURL by default — a var, not a direct reference to the
// const, so tests can point CachedCall/CachedCallStreaming at a fake
// httptest server instead of the real Anthropic API.
var apiURL = APIURL

// ErrCancelled is returned by CachedCallStreaming when cancelled() flips
// true mid-stream — distinct from a real API failure so callers (see
// orchestrator.answerAsk) can tell "superseded" apart from "broken"
// without logging a cancellation as an error.
var ErrCancelled = errors.New("llmcore: call cancelled")

type APIRequest struct {
	Model      string         `json:"model"`
	MaxTokens  int            `json:"max_tokens"`
	Stream     bool           `json:"stream,omitempty"`
	System     []SystemBlock  `json:"system"`
	Messages   []APIMessage   `json:"messages"`
	Tools      []Tool         `json:"tools,omitempty"`
	ToolChoice map[string]any `json:"tool_choice,omitempty"`
}

// SystemBlock is one entry in the API's typed system array. The array
// form (as opposed to a bare string) is what lets us attach cache_control
// to the system prompt for server-side prefix caching.
type SystemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl marks a cache breakpoint. Everything in the request up to
// and INCLUDING the block that carries this marker is server-side cached
// for the TTL window; on the next call with the same prefix, that portion
// bills at ~10% of the normal input rate. Request-order for cache prefix
// is tools → system → messages, so a marker on the system block caches
// tools + system together — which is our whole stable prefix (each of
// arc/router/expander/summarizer/classifier/asker uses one fixed system
// + one fixed tool schema; only the user message varies).
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type APIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type apiResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// PruneLRU caps the cache directory at maxFiles by removing the
// oldest-by-mtime files. No-op if maxFiles <= 0 or the dir doesn't exist.
// Returns the number of files removed.
//
// mtime ≈ first-write for cache files (write-once-read-many), so this is
// effectively a FIFO cap. Good enough — open-mic produces a steady stream
// of unique entries and we just need bounded growth.
func PruneLRU(cacheDir string, maxFiles int) (int, error) {
	if maxFiles <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	type fileInfo struct {
		path  string
		mtime time.Time
	}
	files := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 6 || name[len(name)-5:] != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{filepath.Join(cacheDir, name), info.ModTime()})
	}
	if len(files) <= maxFiles {
		return 0, nil
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].mtime.Before(files[j].mtime)
	})
	toRemove := len(files) - maxFiles
	removed := 0
	for i := 0; i < toRemove; i++ {
		if err := os.Remove(files[i].path); err == nil {
			removed++
		}
	}
	return removed, nil
}

// CacheKey returns a 16-char hex prefix of the sha256 of the parts joined
// by NUL bytes. NUL is unambiguous as a separator because none of the
// inputs (utterance text, JSON-marshaled state) ever contain it.
func CacheKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// CachedCall makes a single tool-forced Messages API call and caches the
// tool-use input under cacheDir/<cid>.json. If the cache file exists and
// parses, the API call is skipped entirely.
//
// The 1024 max-tokens, 60s client timeout, indented cache write, and
// tool_choice={type:tool, name:tool.Name} forcing are all part of the
// cache contract — don't change them without versioning the cache key.
func CachedCall(apiKey, model, system, userText string, tool Tool, cacheDir, cid string) (map[string]any, error) {
	cachePath := filepath.Join(cacheDir, cid+".json")
	if data, err := os.ReadFile(cachePath); err == nil {
		var out map[string]any
		if err := json.Unmarshal(data, &out); err == nil {
			return out, nil
		}
	}
	req := APIRequest{
		Model:     model,
		MaxTokens: 1024,
		System: []SystemBlock{{
			Type:         "text",
			Text:         system,
			CacheControl: &CacheControl{Type: "ephemeral"},
		}},
		Messages: []APIMessage{{Role: "user", Content: userText}},
		Tools:    []Tool{tool},
		ToolChoice: map[string]any{
			"type": "tool",
			"name": tool.Name,
		},
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(context.Background(), "POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", APIVersion)
	httpReq.Header.Set("content-type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("api %d: %s", resp.StatusCode, string(respBody))
	}
	var ar apiResponse
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return nil, fmt.Errorf("parse: %w body=%s", err, string(respBody))
	}
	if ar.Error != nil {
		return nil, fmt.Errorf("api error: %s — %s", ar.Error.Type, ar.Error.Message)
	}
	for _, b := range ar.Content {
		if b.Type == "tool_use" {
			var out map[string]any
			if err := json.Unmarshal(b.Input, &out); err != nil {
				return nil, err
			}
			pretty, _ := json.MarshalIndent(out, "", "  ")
			_ = os.WriteFile(cachePath, pretty, 0o600)
			return out, nil
		}
	}
	return nil, errors.New("no tool_use block in response")
}

// jsonStringFieldDecoder incrementally extracts and unescapes the string
// value of one field from a growing buffer of raw JSON text — built for
// CachedCallStreaming, where the tool's entire input is
// `{"<field>":<optional whitespace>"<value>"}` and nothing else needs
// tracking. It is NOT a general JSON parser and is only correct when the
// target field really is the object's only property (see
// CachedCallStreaming's doc comment).
//
// The API's actual streamed tool-input JSON puts a space after the colon
// (`"reply": "text`, confirmed live 2026-08-25 via raw partial_json
// logging) — an early version of this decoder matched only the
// zero-whitespace form (`"reply":"`), which never matched, so found never
// flipped true and onDelta was never called for the entire stream, on
// every single call, while the separate whitespace-tolerant final
// json.Unmarshal on the full buffer (see CachedCallStreaming) decoded the
// reply text correctly regardless — the reply was always right and the
// spoken audio was always silent. keyPrefix matches just `"<field>":`
// (the colon, nothing after); any whitespace before the opening quote is
// then skipped explicitly, tolerating it arriving split across multiple
// Feed calls the same way keyPrefix itself already did.
//
// Multi-byte UTF-8 characters need no special handling here even when a
// network chunk splits one mid-character: every continuation/lead byte of
// a non-ASCII UTF-8 character is >= 0x80, which can never collide with the
// two bytes this decoder treats specially (`"` = 0x22, `\` = 0x5C), so
// those bytes pass through untouched regardless of where a chunk boundary
// falls. \uXXXX escapes above the Basic Multilingual Plane (surrogate
// pairs) are not reassembled into a single rune — an accepted gap for
// short spoken-reply text, where such escapes are vanishingly unlikely.
type jsonStringFieldDecoder struct {
	keyPrefix string
	foundKey  bool // found `"<field>":`; still skipping whitespace before the opening quote
	inValue   bool // found the opening quote; now inside the string value
	done      bool
	raw       strings.Builder
	pending   []byte
}

func newJSONStringFieldDecoder(field string) *jsonStringFieldDecoder {
	return &jsonStringFieldDecoder{keyPrefix: `"` + field + `":`}
}

// Feed appends newly arrived raw (possibly partial) JSON text and returns
// any newly decoded, unescaped text that became available as a result.
func (d *jsonStringFieldDecoder) Feed(raw string) string {
	if d.done {
		return ""
	}
	if !d.foundKey {
		d.raw.WriteString(raw)
		buf := d.raw.String()
		idx := strings.Index(buf, d.keyPrefix)
		if idx < 0 {
			return ""
		}
		d.foundKey = true
		raw = buf[idx+len(d.keyPrefix):]
		d.raw.Reset()
	}
	if !d.inValue {
		d.raw.WriteString(raw)
		buf := d.raw.String()
		i := 0
		for i < len(buf) && (buf[i] == ' ' || buf[i] == '\t' || buf[i] == '\n' || buf[i] == '\r') {
			i++
		}
		if i >= len(buf) {
			// Nothing but whitespace seen so far — the opening quote
			// hasn't arrived yet.
			return ""
		}
		if buf[i] != '"' {
			// Not a JSON string value where one was expected — give up
			// rather than decode garbage.
			d.done = true
			return ""
		}
		d.inValue = true
		raw = buf[i+1:]
		d.raw.Reset()
	}
	return d.decode(raw)
}

func (d *jsonStringFieldDecoder) decode(s string) string {
	combined := append(d.pending, s...)
	d.pending = nil
	var out strings.Builder
	i := 0
	for i < len(combined) {
		c := combined[i]
		if c == '"' {
			d.done = true
			return out.String()
		}
		if c != '\\' {
			out.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(combined) {
			d.pending = append(d.pending, combined[i:]...)
			break
		}
		esc := combined[i+1]
		switch esc {
		case '"', '\\', '/':
			out.WriteByte(esc)
			i += 2
		case 'n':
			out.WriteByte('\n')
			i += 2
		case 't':
			out.WriteByte('\t')
			i += 2
		case 'r':
			out.WriteByte('\r')
			i += 2
		case 'b':
			out.WriteByte('\b')
			i += 2
		case 'f':
			out.WriteByte('\f')
			i += 2
		case 'u':
			if i+6 > len(combined) {
				d.pending = append(d.pending, combined[i:]...)
				i = len(combined)
				continue
			}
			if r, err := strconv.ParseUint(string(combined[i+2:i+6]), 16, 32); err == nil {
				out.WriteRune(rune(r))
			}
			i += 6
		default:
			out.WriteByte(esc)
			i += 2
		}
	}
	return out.String()
}

// CachedCallStreaming is CachedCall's streaming sibling for single-field
// tool schemas: streamField names the tool input's one string property
// (e.g. AskerTool's "reply"). On a cache hit, onDelta is called once with
// the full cached value — identical cache format and return value to
// CachedCall, whether this call streamed or hit cache. On a cache miss,
// it makes the same request as CachedCall plus Stream:true, reads the
// response as Server-Sent Events, and calls onDelta with each
// incrementally decoded piece of streamField's value as the model
// generates it (see jsonStringFieldDecoder).
//
// cancelled is polled once per parsed SSE event; if it returns true the
// response body is closed and ErrCancelled is returned immediately,
// without writing to cache — a superseded call stops paying for tokens
// mid-generation instead of running to completion. cancelled may be nil
// (never cancelled).
func CachedCallStreaming(apiKey, model, system, userText string, tool Tool, cacheDir, cid, streamField string, cancelled func() bool, onDelta func(chunk string)) (map[string]any, error) {
	cachePath := filepath.Join(cacheDir, cid+".json")
	if data, err := os.ReadFile(cachePath); err == nil {
		var out map[string]any
		if err := json.Unmarshal(data, &out); err == nil {
			if v, ok := out[streamField].(string); ok && onDelta != nil {
				onDelta(v)
			}
			return out, nil
		}
	}
	if cancelled != nil && cancelled() {
		return nil, ErrCancelled
	}

	req := APIRequest{
		Model:     model,
		MaxTokens: 1024,
		Stream:    true,
		System: []SystemBlock{{
			Type:         "text",
			Text:         system,
			CacheControl: &CacheControl{Type: "ephemeral"},
		}},
		Messages: []APIMessage{{Role: "user", Content: userText}},
		Tools:    []Tool{tool},
		ToolChoice: map[string]any{
			"type": "tool",
			"name": tool.Name,
		},
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(context.Background(), "POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", APIVersion)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("api %d: %s", resp.StatusCode, string(respBody))
	}

	dec := newJSONStringFieldDecoder(streamField)
	var rawInput strings.Builder
	var apiErr error
	var eventType string

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if cancelled != nil && cancelled() {
			return nil, ErrCancelled
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			switch eventType {
			case "content_block_delta":
				var ev struct {
					Delta struct {
						Type        string `json:"type"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(data), &ev); err == nil && ev.Delta.Type == "input_json_delta" {
					rawInput.WriteString(ev.Delta.PartialJSON)
					if chunk := dec.Feed(ev.Delta.PartialJSON); chunk != "" && onDelta != nil {
						onDelta(chunk)
					}
				}
			case "error":
				var ev struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(data), &ev); err == nil {
					apiErr = fmt.Errorf("api error: %s — %s", ev.Error.Type, ev.Error.Message)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SSE stream: %w", err)
	}
	if apiErr != nil {
		return nil, apiErr
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(rawInput.String()), &out); err != nil {
		return nil, fmt.Errorf("decode streamed tool input: %w body=%s", err, rawInput.String())
	}
	pretty, _ := json.MarshalIndent(out, "", "  ")
	_ = os.WriteFile(cachePath, pretty, 0o600)
	return out, nil
}
