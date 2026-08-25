package main

import (
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
)

func TestUtteranceBuffering(t *testing.T) {
	s := &session{}
	s.appendUtterance("hello")
	s.appendUtterance("world")
	got := s.takeUtterance()
	if got != "hello world" {
		t.Errorf("takeUtterance() = %q, want %q", got, "hello world")
	}
	// Draining resets the buffer.
	if got := s.takeUtterance(); got != "" {
		t.Errorf("takeUtterance() after drain = %q, want empty", got)
	}
}

func TestServeWSRejectsMissingOrWrongToken(t *testing.T) {
	srv := &server{authToken: "correct-token"}
	mux, err := srv.routes()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()

	wsURL := "ws" + ts.URL[len("http"):] + "/ws"

	cases := []struct {
		name  string
		token string
	}{
		{"missing token", ""},
		{"wrong token", "wrong"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := wsURL
			if c.token != "" {
				url += "?token=" + c.token
			}
			_, resp, err := websocket.DefaultDialer.Dial(url, nil)
			if err == nil {
				t.Fatal("expected the dial to fail for an unauthorized connection")
			}
			if resp == nil || resp.StatusCode != 401 {
				status := -1
				if resp != nil {
					status = resp.StatusCode
				}
				t.Errorf("status = %d, want 401", status)
			}
		})
	}
}
