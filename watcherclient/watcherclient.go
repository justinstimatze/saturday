// Package watcherclient is the Unix-socket client for saturday-watcher's
// /state endpoint. Extracted from saturday-mayor so any consumer of live
// session state (mayor's own routing pipeline today, a Drive-relay backend
// later) can fetch it without duplicating the HTTP-over-Unix-socket
// plumbing. Wire format matches watcher/main.go's SessionEntry exactly
// (json tags state/last_event_at/jsonl_path/events_seen).
package watcherclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	llm "saturday/llmcore"
)

// SessionEntry is one active session as reported by the watcher.
type SessionEntry struct {
	State       llm.State `json:"state"`
	LastEventAt time.Time `json:"last_event_at"`
	JSONLPath   string    `json:"jsonl_path"`
	EventsSeen  int       `json:"events_seen"`
}

// FetchSessions queries the watcher's /state endpoint over its Unix socket
// and returns every session it currently knows about.
func FetchSessions(sockPath string) ([]SessionEntry, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get("http://x/state")
	if err != nil {
		return nil, fmt.Errorf("watcher %s: %w", sockPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("watcher %s: status %d", sockPath, resp.StatusCode)
	}
	var sessions []SessionEntry
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return sessions, nil
}
