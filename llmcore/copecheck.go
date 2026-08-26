package llmcore

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// CheckVoiceRegister scores text against saturday.effigy using cope-gate —
// the same scan engine (github.com/justinstimatze/cope) that scores Claude
// Code's own replies against claude_voice.effigy via this host's Stop hook.
// cope-gate's --check mode takes plain prose on stdin and a card path via
// --rules; it has no Claude-Code-transcript/session_id dependency, unlike
// cope's hook subcommands, so it's the one piece of cope actually reusable
// for Saturday's own spoken text.
//
// Fire-and-forget, warn-only — mirrors cope-gate's own live wiring for
// Claude Code (the Stop hook runs bare, no -block flag): this never blocks
// or rewrites what gets spoken, only appends scored violations to a local
// log for later review. Callers that don't want to pay even the subprocess
// spawn latency on the speech path should invoke it as `go
// CheckVoiceRegister(text)`.
//
// A missing cope-gate binary, or no resolvable state/cache directory, is
// not an error — optional enrichment, same posture as LoadVocabularyTics.
func CheckVoiceRegister(text string) {
	binPath, err := exec.LookPath("cope-gate")
	if err != nil {
		return
	}
	card := copeCardFile()
	if card == "" {
		return
	}
	logDir := xdgDir("XDG_STATE_HOME", ".local/state", "saturday")
	if logDir == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return
	}

	cmd := exec.Command(binPath, "--check", "-", "--rules", card, "-log", filepath.Join(logDir, "cope-violations.jsonl"))
	cmd.Stdin = bytes.NewReader([]byte(text))
	_ = cmd.Run()
}

var (
	copeCardOnce sync.Once
	copeCardPath string
)

// copeCardFile materializes saturday.effigy's raw, unstripped form (the same
// bytes go:embed'd into saturdayEffigy) to a stable cache path once per
// process. cope-gate's --rules flag needs a real file on disk; writing out
// the embedded string sidesteps any assumption about the running binary's
// working directory or checkout layout, and stays in sync with the compiled
// card automatically since it's the same string EffigyForPrompt reads.
func copeCardFile() string {
	copeCardOnce.Do(func() {
		dir := xdgDir("XDG_CACHE_HOME", ".cache", "saturday")
		if dir == "" {
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
		path := filepath.Join(dir, "saturday.effigy")
		if err := os.WriteFile(path, []byte(saturdayEffigy), 0o644); err != nil {
			return
		}
		copeCardPath = path
	})
	return copeCardPath
}

// xdgDir resolves $envVar, falling back to $HOME/homeRelative, then joins
// app onto it. Returns "" if neither is available.
func xdgDir(envVar, homeRelative, app string) string {
	base := os.Getenv(envVar)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, filepath.FromSlash(homeRelative))
	}
	return filepath.Join(base, app)
}
