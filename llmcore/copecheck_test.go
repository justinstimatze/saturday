package llmcore

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckVoiceRegisterMissingBinaryIsNoop(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no cope-gate reachable
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	CheckVoiceRegister("this should not panic or error") // no assertion beyond "doesn't blow up"
}

func TestCheckVoiceRegisterLogsAViolation(t *testing.T) {
	if _, err := exec.LookPath("cope-gate"); err != nil {
		t.Skip("cope-gate not installed on this machine")
	}
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	CheckVoiceRegister("Lucida tests done — sorry for the delay.")

	log, err := os.ReadFile(filepath.Join(stateDir, "saturday", "cope-violations.jsonl"))
	if err != nil {
		t.Fatalf("expected a violations log, got: %v", err)
	}
	if !strings.Contains(string(log), "apology") {
		t.Errorf("expected an apology violation logged, got:\n%s", log)
	}
}
