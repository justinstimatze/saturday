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
		{State: llm.State{Project: "spar", SessionArc: "designing an error-injection tool"}},
		{State: llm.State{Project: "lucida", LastAssistantText: "documented WebGL perf"}},
		{State: llm.State{Project: "adit"}},
	}

	got := buildManifestContent(sessions, now)

	if !strings.Contains(got, "2026-08-24T21:00:00Z") {
		t.Errorf("expected the update timestamp in output, got:\n%s", got)
	}
	if !strings.Contains(got, "3 live") {
		t.Errorf("expected session count in output, got:\n%s", got)
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

func TestBuildManifestContentEmpty(t *testing.T) {
	got := buildManifestContent(nil, time.Date(2026, 8, 24, 21, 0, 0, 0, time.UTC))
	if !strings.Contains(got, "0 live") {
		t.Errorf("expected '0 live' for no sessions, got:\n%s", got)
	}
}
