// saturday-backend — Drive-relay backend for Saturday Native (Phase 1).
//
// Polls a private Google Drive folder for plain-language notes written
// there by Claude's voice mode via its first-party Drive connector (see
// SATURDAY-VOICE-NATIVE.md §2), routes each note to the right live Claude
// Code session using the same router/expander pipeline saturday-mayor runs
// for voice utterances, and injects it using Phase 0's extracted
// inject/settle/watcherclient packages.
//
// Auth is a one-time OAuth consent (`--drive-login`); everyday runs are
// fully headless off the cached token. See README.md for the Google Cloud
// Console setup steps only the user can do.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	llm "saturday/llmcore"
)

var version = "dev"

// xdgConfigHome mirrors saturday-mayor's own helper (each binary keeps its
// own trivial copy rather than sharing a package for ~10 lines — same
// precedent as sync/main.go and saturday-mayor/main.go's local `head()`).
func xdgConfigHome() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config")
	}
	return ""
}

func xdgCacheHome() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return x
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cache")
	}
	return ""
}

func oneLine(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// pollOnce fetches every note created since c.LastProcessed, skips any
// already in c.RecentIDs, processes each in creation order, and returns the
// advanced cursor. A per-note processing failure is logged and the note is
// still marked seen — matching this codebase's existing fire-and-forget
// tolerance (no retry loop anywhere in saturday-mayor either); a note that
// can't be handled shouldn't wedge every note behind it forever.
func pollOnce(ctx context.Context, src driveSource, c cursor, process func(text string) error) cursor {
	notes, err := src.ListNew(ctx, c.LastProcessed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "poll: list notes: %v\n", err)
		return c
	}
	for _, n := range notes {
		if c.seen(n.ID) {
			continue
		}
		fmt.Fprintf(os.Stderr, "← note %s (%s): %q\n", n.ID, n.CreatedTime.Format(time.RFC3339), oneLine(n.Text, 120))
		if err := process(n.Text); err != nil {
			fmt.Fprintf(os.Stderr, "  ↳ process failed: %v\n", err)
		}
		c = c.advance(n.ID, n.CreatedTime)
	}
	return c
}

func main() {
	defaultEnv := ""
	if cfg := xdgConfigHome(); cfg != "" {
		defaultEnv = filepath.Join(cfg, "saturday", "config")
	}
	defaultCache := ""
	if cacheBase := xdgCacheHome(); cacheBase != "" {
		defaultCache = filepath.Join(cacheBase, "saturday", "llm")
	}
	defaultCreds := ""
	defaultToken := ""
	defaultCursor := ""
	if cfg := xdgConfigHome(); cfg != "" {
		defaultCreds = filepath.Join(cfg, "saturday", "drive-credentials.json")
		defaultToken = filepath.Join(cfg, "saturday", "drive-token.json")
		defaultCursor = filepath.Join(cfg, "saturday", "backend-cursor.json")
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	sock := flag.String("sock", "/tmp/saturday-watcher.sock", "watcher Unix socket path")
	envPath := flag.String("env", defaultEnv, ".env file with ANTHROPIC_API_KEY")
	cacheDir := flag.String("cache", defaultCache, "directory for cached LLM responses")
	dryRun := flag.Bool("dry-run", false, "log proposals but do not exec claude --resume --print")
	collisionWait := flag.Duration("collision-wait", 500*time.Millisecond, "JSONL must be size-stable for this long before injecting")
	collisionMax := flag.Duration("collision-max", 5*time.Second, "give up waiting and inject anyway after this")
	confThreshold := flag.Float64("conf-threshold", 0.5, "skip inject if router or expander confidence <= this (must exceed to proceed); 0 disables")
	injectDirectTokens := flag.Int("inject-direct-threshold", 80000, "if est. tokens since last isCompactSummary in target JSONL exceed this, direct-write instead of headless inject; 0 disables")
	credsPath := flag.String("drive-credentials", defaultCreds, "OAuth 2.0 Desktop-app client secret JSON, downloaded from Google Cloud Console (see README.md)")
	tokenPath := flag.String("drive-token", defaultToken, "cached OAuth token — created by --drive-login, read on every normal run")
	folderID := flag.String("drive-folder-id", "", "Drive folder ID to poll (the folder ID segment from its URL)")
	pollInterval := flag.Duration("poll-interval", 15*time.Second, "how often to check the Drive folder for new notes")
	cursorPath := flag.String("cursor", defaultCursor, "cursor state file (what's already been processed)")
	manifestName := flag.String("drive-manifest-name", "saturday-sessions.txt", "filename for the live-session inventory this backend writes back to the Drive folder, so voice mode can check real session names before writing a note; empty disables it (requires the drive.file scope — re-run --drive-login after upgrading from a readonly-only token)")
	stageSock := flag.String("stage-sock", "", "if set, dial this Unix socket and send focus/restore commands to the saturday-stage window-choreography sidecar on the inject lifecycle. Empty = disabled (no window choreography).")
	stageZoom := flag.Bool("stage-zoom", false, "Posture A (cockpit): on inject, ask stage to zoom (maximize) the addressed pane; restore unzooms. Takes precedence over --stage-tile.")
	stageTile := flag.Bool("stage-tile", false, "Posture A (cockpit): on inject, ask stage to give the addressed pane a proportionally larger share of an even-horizontal row (salience tiling); restore re-evens.")
	stageRestorePoll := flag.Duration("stage-restore-poll", 3*time.Second, "how often to check whether a tmux-injected reply has finished, before telling stage to restore the pane")
	stageRestoreMaxWait := flag.Duration("stage-restore-max-wait", 5*time.Minute, "give up waiting for the reply to finish and restore the pane anyway after this long")
	drivelogin := flag.Bool("drive-login", false, "run the one-time interactive OAuth consent flow, save the token, and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if *drivelogin {
		cfg, err := loadOAuthConfig(*credsPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load OAuth config:", err)
			os.Exit(1)
		}
		if err := runLogin(cfg, *tokenPath); err != nil {
			fmt.Fprintln(os.Stderr, "login:", err)
			os.Exit(1)
		}
		return
	}

	if *folderID == "" {
		fmt.Fprintln(os.Stderr, "--drive-folder-id is required")
		os.Exit(1)
	}

	llm.LoadDotEnv(*envPath)
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY not set (checked env and "+*envPath+")")
		os.Exit(1)
	}
	if err := os.MkdirAll(*cacheDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir cache:", err)
		os.Exit(1)
	}

	cfg, err := loadOAuthConfig(*credsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load OAuth config:", err)
		os.Exit(1)
	}
	tok, err := loadToken(*tokenPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load token:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	src, err := newDriveClient(ctx, cfg, tok, *folderID, *manifestName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "drive client:", err)
		os.Exit(1)
	}

	b := &backend{
		apiKey:              apiKey,
		sockPath:            *sock,
		cacheDir:            *cacheDir,
		dryRun:              *dryRun,
		collisionWait:       *collisionWait,
		collisionMax:        *collisionMax,
		confThreshold:       *confThreshold,
		injectDirectTokens:  *injectDirectTokens,
		stageZoom:           *stageZoom,
		stageTile:           *stageTile,
		stageRestorePoll:    *stageRestorePoll,
		stageRestoreMaxWait: *stageRestoreMaxWait,
	}
	if *stageSock != "" {
		go b.stage.Run(*stageSock)
	}

	c := loadCursor(*cursorPath)
	fmt.Fprintf(os.Stderr, "saturday-backend — folder=%s poll=%s dry-run=%v\n", *folderID, *pollInterval, *dryRun)

	tick := time.NewTicker(*pollInterval)
	defer tick.Stop()
	for {
		c = pollOnce(ctx, src, c, b.processNote)
		saveCursor(*cursorPath, c)
		if *manifestName != "" && !*dryRun {
			n, err := refreshManifest(ctx, src, *sock)
			if err != nil {
				fmt.Fprintf(os.Stderr, "manifest: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "↻ manifest: %d live session(s) → %s\n", n, *manifestName)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
