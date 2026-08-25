package stageclient

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestClientWriteNoopWhenDisconnected(t *testing.T) {
	var c Client
	// Zero-value Client: no Run launched, conn is nil. Write must not panic
	// or block — this is the behavior a caller with no --stage-sock relies
	// on for every inject.
	c.Write(map[string]any{"type": "focus", "session_id": "abc"})
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		t.Fatalf("expected conn to stay nil with no Run launched, got %v", conn)
	}
}

func TestClientRunDialsAndWrites(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "stage.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	var c Client
	go c.Run(sock)

	var serverConn net.Conn
	select {
	case serverConn = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Run to dial")
	}
	defer serverConn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		connected := c.conn != nil
		c.mu.Unlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Client.conn to be set")
		}
		time.Sleep(10 * time.Millisecond)
	}

	c.Write(map[string]any{"type": "focus", "session_id": "sess-1", "zoom": true})

	serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(serverConn).ReadString('\n')
	if err != nil {
		t.Fatalf("read from client: %v", err)
	}
	var evt map[string]any
	if err := json.Unmarshal([]byte(line), &evt); err != nil {
		t.Fatalf("unmarshal written line: %v", err)
	}
	if evt["type"] != "focus" || evt["session_id"] != "sess-1" || evt["zoom"] != true {
		t.Fatalf("unexpected event written: %#v", evt)
	}
}
