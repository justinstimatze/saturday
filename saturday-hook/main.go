// saturday-hook — bridge from Claude Code's UserPromptSubmit / Stop hooks
// into mayor's hook socket.
//
// Wired in ~/.claude/settings.json:
//
//	"UserPromptSubmit": [{ "hooks": [{ "type": "command",
//	   "command": "/home/gas6amus/go/bin/saturday-hook prompt" }] }]
//	"Stop": [{ "hooks": [{ "type": "command",
//	   "command": "/home/gas6amus/go/bin/saturday-hook stop" }] }]
//
// CC pipes the hook event payload as JSON on stdin. Shape (per CC docs):
//
//	{"session_id":"…","cwd":"…","prompt":"…","transcript_path":"…", …}
//
// Behavior contract:
//   - Read stdin (cap 1 MB). Parse top-level JSON.
//   - Append "event": "prompt_submit"|"stop", drop oversized fields, send
//     one line to /tmp/saturday-mayor-hooks.sock. Never block CC.
//   - Hard 250 ms total deadline. If mayor is down, the dial errors instantly
//     (no socket file) — exit 0 so CC's hook chain continues.
//   - Any failure path exits 0. This binary is best-effort observability;
//     it must never break the user's CC session.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

const (
	defaultSock   = "/tmp/saturday-mayor-hooks.sock"
	stdinReadCap  = 1 << 20 // 1 MB; CC prompts larger than this would already be unusual
	dialDeadline  = 100 * time.Millisecond
	writeDeadline = 250 * time.Millisecond
	// Trim long fields before forwarding; mayor only needs enough to
	// correlate (prompt prefix) and dispatch (session_id).
	promptCap = 4096
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(0)
	}
	var event string
	switch os.Args[1] {
	case "prompt":
		event = "prompt_submit"
	case "stop":
		event = "stop"
	default:
		os.Exit(0)
	}

	// Read CC's hook payload from stdin.
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, stdinReadCap))
	if err != nil || len(raw) == 0 {
		os.Exit(0)
	}

	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		os.Exit(0)
	}

	out := map[string]any{
		"event": event,
		"ts":    float64(time.Now().UnixNano()) / 1e9,
	}
	if v, ok := in["session_id"].(string); ok {
		out["session_id"] = v
	}
	if v, ok := in["cwd"].(string); ok {
		out["cwd"] = v
	}
	if v, ok := in["prompt"].(string); ok {
		if len(v) > promptCap {
			v = v[:promptCap]
		}
		out["prompt"] = v
	}
	if v, ok := in["transcript_path"].(string); ok {
		out["transcript_path"] = v
	}

	body, err := json.Marshal(out)
	if err != nil {
		os.Exit(0)
	}
	body = append(body, '\n')

	sock := os.Getenv("SATURDAY_HOOK_SOCK")
	if sock == "" {
		sock = defaultSock
	}

	d := net.Dialer{Timeout: dialDeadline}
	conn, err := d.Dial("unix", sock)
	if err != nil {
		os.Exit(0)
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(writeDeadline))
	if _, err := conn.Write(body); err != nil {
		fmt.Fprintln(os.Stderr, "saturday-hook: write:", err)
	}
}
