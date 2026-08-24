// Package settle detects when a live Claude Code session's JSONL transcript
// has gone quiet — either "quiet enough to safely inject into" (WaitForQuiet)
// or "quiet because the assistant's reply to a specific inject finished"
// (AssistantTextAfterInject). Extracted from saturday-mayor so any injector
// (mayor's tmux/audio pipeline today, a Drive-relay backend later) can reuse
// the same JSONL-stability primitives without duplicating them.
package settle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// WaitForQuiet defers until jsonlPath has been size-stable for quietFor, or
// aborts the wait at maxWait. JSONL writes from a live `claude` process come
// in bursts (assistant streaming, tool chains); waiting for a quiet window
// minimizes the chance a concurrent inject interleaves mid-turn with those
// writes. Returns waited duration and whether the wait hit maxWait.
func WaitForQuiet(jsonlPath string, quietFor, maxWait time.Duration) (time.Duration, bool) {
	if quietFor <= 0 || jsonlPath == "" {
		return 0, false
	}
	start := time.Now()
	var lastSize int64 = -1
	var stableSince time.Time
	for {
		info, err := os.Stat(jsonlPath)
		if err != nil {
			// missing JSONL = nothing to collide with; proceed.
			return time.Since(start), false
		}
		if info.Size() != lastSize {
			lastSize = info.Size()
			stableSince = time.Now()
		} else if time.Since(stableSince) >= quietFor {
			return time.Since(start), false
		}
		if time.Since(start) >= maxWait {
			return time.Since(start), true
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// FileSize is a small os.Stat wrapper — kept here since both WaitForQuiet's
// caller and an inject-completion tracker need the same "how big is this
// JSONL right now" primitive.
func FileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// AssistantTextAfterInject scans the JSONL forward from fromOff and returns
// the text of the last assistant block that follows a user message
// containing injectText. ready=false means inject hasn't surfaced as a user
// message yet (still queued behind unrelated work) OR the most recent
// assistant block after the user-message is still tool_use/thinking (chain
// in progress) — caller should keep polling.
//
// The user-message gate is what makes this correct under "inject queued
// behind a previous task": pre-inject assistant text is ignored even if
// it's freshly written, because it predates the injected user message.
//
// CC stores actual user input as message.content="<string>"; tool_results
// arrive as content=[{"type":"tool_result",...}]. Only string-form user
// content is matched — tool_results never carry injectText anyway.
//
// fromOff is the JSONL size at the moment the inject went out. json.Valid
// drops any partial line the scan lands in mid-record.
func AssistantTextAfterInject(jsonlPath string, fromOff int64, injectText string) (string, bool, error) {
	needle := strings.ToLower(strings.TrimSpace(injectText))
	if needle == "" {
		return "", false, nil
	}
	f, err := os.Open(jsonlPath)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	seekFrom := fromOff
	if seekFrom > info.Size() {
		return "", false, nil
	}
	if _, err := f.Seek(seekFrom, 0); err != nil {
		return "", false, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	userSeen := false
	var lastAssistantLine []byte
	for sc.Scan() {
		line := sc.Bytes()
		if !json.Valid(line) {
			continue
		}
		if bytes.Contains(line, []byte(`"type":"user"`)) {
			var ev struct {
				Type    string `json:"type"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &ev) != nil || ev.Type != "user" {
				continue
			}
			var s string
			if json.Unmarshal(ev.Message.Content, &s) != nil {
				continue // array form (tool_result), not human input
			}
			if strings.Contains(strings.ToLower(s), needle) {
				userSeen = true
				lastAssistantLine = nil
			}
			continue
		}
		if !userSeen {
			continue
		}
		if !bytes.Contains(line, []byte(`"type":"assistant"`)) {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		lastAssistantLine = cp
	}
	if err := sc.Err(); err != nil {
		return "", false, err
	}
	if !userSeen || lastAssistantLine == nil {
		return "", false, nil
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lastAssistantLine, &ev); err != nil {
		return "", false, err
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(ev.Message.Content, &blocks); err == nil && len(blocks) > 0 {
		last := blocks[len(blocks)-1]
		if last.Type == "text" {
			return last.Text, true, nil
		}
		return "", false, nil // tool_use / thinking — chain still running
	}
	var s string
	if err := json.Unmarshal(ev.Message.Content, &s); err == nil {
		return s, true, nil
	}
	return "", false, nil
}
