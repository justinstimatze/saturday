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

type APIRequest struct {
	Model      string         `json:"model"`
	MaxTokens  int            `json:"max_tokens"`
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
	httpReq, err := http.NewRequestWithContext(context.Background(), "POST", APIURL, bytes.NewReader(body))
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
			_ = os.WriteFile(cachePath, pretty, 0o644)
			return out, nil
		}
	}
	return nil, errors.New("no tool_use block in response")
}
