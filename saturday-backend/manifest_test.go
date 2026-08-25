package main

import (
	"strings"
	"testing"
	"time"

	llm "saturday/llmcore"
	"saturday/watcherclient"
)

func TestBuildManifestContentSortedAndSummarized(t *testing.T) {
	now := time.Date(2026, 8, 24, 21, 0, 0, 0, time.UTC)
	sessions := []watcherclient.SessionEntry{
		{State: llm.State{Project: "spar", Cwd: "/home/j/spar", SessionArc: "designing an error-injection tool"}},
		{State: llm.State{Project: "lucida", Cwd: "/home/j/lucida", LastAssistantText: "documented WebGL perf"}},
		{State: llm.State{Project: "adit", Cwd: "/home/j/adit"}},
	}
	// All three reachable via tmux for this test — sort/summary behavior is
	// independent of the tmux/headless split, covered separately below.
	tmuxLive := map[string]bool{"/home/j/spar": true, "/home/j/lucida": true, "/home/j/adit": true}

	got := buildManifestContent(sessions, tmuxLive, now)

	if !strings.Contains(got, "2026-08-24T21:00:00Z") {
		t.Errorf("expected the update timestamp in output, got:\n%s", got)
	}
	if !strings.Contains(got, "3 live (3 reachable now, 0 headless-only)") {
		t.Errorf("expected session/reachability counts in output, got:\n%s", got)
	}
	// Sorted by project name: adit, lucida, spar.
	adit := strings.Index(got, "adit —")
	lucida := strings.Index(got, "lucida —")
	spar := strings.Index(got, "spar —")
	if adit == -1 || lucida == -1 || spar == -1 || !(adit < lucida && lucida < spar) {
		t.Errorf("expected adit, lucida, spar in sorted order, got:\n%s", got)
	}
	if !strings.Contains(got, "designing an error-injection tool") {
		t.Errorf("expected spar's SessionArc as its summary, got:\n%s", got)
	}
	if !strings.Contains(got, "documented WebGL perf") {
		t.Errorf("expected lucida's LastAssistantText fallback since it has no SessionArc, got:\n%s", got)
	}
	if !strings.Contains(got, "adit — (no summary yet)") {
		t.Errorf("expected adit's placeholder since it has neither SessionArc nor LastAssistantText, got:\n%s", got)
	}
}

func TestBuildManifestContentSplitsByTmuxReachability(t *testing.T) {
	now := time.Date(2026, 8, 24, 21, 0, 0, 0, time.UTC)
	sessions := []watcherclient.SessionEntry{
		{State: llm.State{Project: "spar", Cwd: "/home/j/spar"}},     // has a pane
		{State: llm.State{Project: "lucida", Cwd: "/home/j/lucida"}}, // watcher-live, no pane
	}
	tmuxLive := map[string]bool{"/home/j/spar": true}

	got := buildManifestContent(sessions, tmuxLive, now)

	if !strings.Contains(got, "2 live (1 reachable now, 1 headless-only)") {
		t.Errorf("expected 1/1 split in the header, got:\n%s", got)
	}
	reachable := strings.Index(got, "Reachable now")
	headless := strings.Index(got, "Live but no tmux pane")
	sparIdx := strings.Index(got, "spar —")
	lucidaIdx := strings.Index(got, "lucida —")
	if reachable == -1 || headless == -1 || sparIdx == -1 || lucidaIdx == -1 {
		t.Fatalf("expected both section headers and both sessions present, got:\n%s", got)
	}
	if !(reachable < sparIdx && sparIdx < headless) {
		t.Errorf("expected spar under the 'Reachable now' section, before 'headless-only', got:\n%s", got)
	}
	if !(headless < lucidaIdx) {
		t.Errorf("expected lucida under the headless-only section, got:\n%s", got)
	}
}

func TestBuildManifestContentEmpty(t *testing.T) {
	got := buildManifestContent(nil, nil, time.Date(2026, 8, 24, 21, 0, 0, 0, time.UTC))
	if !strings.Contains(got, "0 live (0 reachable now, 0 headless-only)") {
		t.Errorf("expected all-zero counts for no sessions, got:\n%s", got)
	}
	if !strings.Contains(got, "(none)") {
		t.Errorf("expected '(none)' placeholder in both empty sections, got:\n%s", got)
	}
}
