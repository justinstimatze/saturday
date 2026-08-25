// Package stageclient dials the saturday-stage window-choreography sidecar
// and writes focus/restore commands from an inject lifecycle. Extracted
// from saturday-mayor (its original owner) so saturday-backend's
// Drive-relay inject path can dial the same sidecar the same way, without
// either binary depending on the other's package main.
package stageclient

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// Client serializes JSON command writes to the stage sidecar. The zero
// value is usable and permanently no-ops (Write returns immediately) until
// Run is launched against a real socket path — callers that don't set
// stage up at all (no --stage-sock) simply never call Run, and every
// Write stays a no-op, same as stage being down.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
}

// Write marshals evt as one JSON line and writes it to the sidecar. No-op
// if not connected, so the inject path never blocks or errors on stage
// being absent or down. On write failure the conn is dropped; Run
// reconnects.
func (c *Client) Write(evt map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return
	}
	b, _ := json.Marshal(evt)
	b = append(b, '\n')
	if _, err := c.conn.Write(b); err != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// Run dials sockPath and keeps the connection live, reconnecting with
// capped backoff (stage may start after the caller, or restart under it).
// It reads and discards anything stage sends (the window_activity stream)
// purely to detect disconnects; the caller is a command producer here.
// Meant to be launched with `go client.Run(sockPath)`.
func (c *Client) Run(sockPath string) {
	backoff := time.Second
	for {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			time.Sleep(backoff)
			if backoff < 16*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
		fmt.Fprintf(os.Stderr, "\033[2m  stage sidecar connected (%s)\033[0m\n", sockPath)

		buf := make([]byte, 4096)
		for {
			if _, err := conn.Read(buf); err != nil {
				break
			}
		}
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		conn.Close()
		fmt.Fprintln(os.Stderr, "\033[2m  stage sidecar disconnected\033[0m")
	}
}
