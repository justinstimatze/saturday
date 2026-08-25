// saturday-voice talks directly to Kyutai's moshi-server STT/TTS over
// their native msgpack/WebSocket protocol (see saturday/moshiclient) and
// drives Saturday's own ask/route/inject/summarize decision core (see
// saturday/orchestrator) — no Unmute backend or frontend in this build;
// Unmute was Phase 0's validation harness only.
//
// Serves a minimal static client (getUserMedia capture, WebSocket audio
// streaming, Web Audio playback) at "/", and the client-facing audio
// WebSocket at "/ws". One goroutine (a *session, see pipeline.go) per
// connected client; all cross-session voice-routing state lives in the
// shared *orchestrator.Orchestrator, not per-session.
package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/websocket"

	llm "saturday/llmcore"
	"saturday/moshiclient"
	"saturday/orchestrator"
)

//go:embed static
var staticFS embed.FS

// xdgConfigHome / xdgCacheHome mirror saturday-mayor's own resolution —
// same conventions, so a shared .env/cache dir works for both without
// extra configuration.
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

func main() {
	defaultEnv := ""
	if cfg := xdgConfigHome(); cfg != "" {
		defaultEnv = filepath.Join(cfg, "saturday", "config")
	}
	defaultCache := ""
	if cacheBase := xdgCacheHome(); cacheBase != "" {
		defaultCache = filepath.Join(cacheBase, "saturday", "llm")
	}

	port := flag.String("port", "8080", "HTTP port to listen on (serves the static client and the /ws audio endpoint)")
	authToken := flag.String("auth-token", "", "required bearer token, passed by the client as ?token=... on the /ws connection (browsers can't set custom WebSocket headers) — this endpoint can act on a live coding session, so this is mandatory, not optional")
	sttURL := flag.String("moshi-stt-url", "http://localhost:8090", "base URL of moshi-server's STT service (e.g. https://<pod>.proxy.runpod.net/stt-raw)")
	ttsURL := flag.String("moshi-tts-url", "http://localhost:8089", "base URL of moshi-server's TTS service (e.g. https://<pod>.proxy.runpod.net/tts-raw)")
	moshiAPIKey := flag.String("moshi-api-key", "", "kyutai-api-key header value for moshi-server, if it requires one")
	voice := flag.String("voice", "unmute-prod-website/ex04_narration_longform_00001.wav", "TTS voice identifier")
	watcherSock := flag.String("watcher-sock", "/tmp/saturday-watcher.sock", "watcher Unix socket path")
	envPath := flag.String("env", defaultEnv, ".env file with ANTHROPIC_API_KEY")
	cacheDir := flag.String("cache", defaultCache, "directory for cached LLM responses")
	confThreshold := flag.Float64("conf-threshold", 0.5, "skip inject if router or expander confidence <= this; 0 disables")
	askConf := flag.Float64("ask-conf", 0.7, "classifier conf threshold to route to ask-mode instead of inject-mode")
	collisionWait := flag.Duration("collision-wait", 500*time.Millisecond, "JSONL must be size-stable for this long before injecting")
	collisionMax := flag.Duration("collision-max", 5*time.Second, "give up waiting and inject anyway after this")
	injectDirectTokens := flag.Int("inject-direct-threshold", 80000, "est. tokens since last compact above which mayor direct-writes instead of headless-injecting; 0 disables")
	stabilityWindow := flag.Duration("stability-window", 5*time.Second, "Phase 3: target JSONL must be size-stable this long before considering a tracked inject complete")
	completionTTL := flag.Duration("completion-ttl", 10*time.Minute, "Phase 3: drop tracked injects that haven't completed within this window")
	minGrowthBytes := flag.Int64("min-growth", 200, "Phase 3: skip completion report if JSONL grew less than this many bytes")
	minElapsed := flag.Duration("min-elapsed", 5*time.Second, "Phase 3: skip completion report if less than this elapsed since inject")
	arcInterval := flag.Duration("arc-interval", 5*time.Minute, "cadence of the slow-loop session-arc summarizer; 0 disables")
	noBlink := flag.Bool("no-blink", false, "disable the das-blinkenlights corner tag on injected sessions")
	stageSock := flag.String("stage-sock", "", "if set, dial this Unix socket and send focus/restore commands to saturday-stage on the inject lifecycle")
	stageZoom := flag.Bool("stage-zoom", false, "on inject, ask stage to zoom the addressed pane")
	stageTile := flag.Bool("stage-tile", false, "on inject, ask stage to salience-tile the addressed pane")
	dryRun := flag.Bool("dry-run", false, "log proposals but don't actually commit an inject")
	flag.Parse()

	if *authToken == "" {
		fmt.Fprintln(os.Stderr, "--auth-token is required: this endpoint can act on a live coding session")
		os.Exit(1)
	}

	llm.LoadDotEnv(*envPath)
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY not set (checked env and "+*envPath+")")
		os.Exit(1)
	}
	if *cacheDir != "" {
		if err := os.MkdirAll(*cacheDir, 0o750); err != nil {
			fmt.Fprintln(os.Stderr, "mkdir cache:", err)
			os.Exit(1)
		}
	}

	srv := &server{
		authToken:   *authToken,
		sttURL:      *sttURL,
		ttsURL:      *ttsURL,
		moshiAPIKey: *moshiAPIKey,
		voice:       moshiclient.TTSVoice(*voice),
		orchTemplate: orchestrator.Config{
			APIKey:             apiKey,
			CacheDir:           *cacheDir,
			WatcherSock:        *watcherSock,
			ConfThreshold:      *confThreshold,
			AskConf:            *askConf,
			CollisionWait:      *collisionWait,
			CollisionMax:       *collisionMax,
			InjectDirectTokens: *injectDirectTokens,
			StabilityWindow:    *stabilityWindow,
			CompletionTTL:      *completionTTL,
			MinGrowthBytes:     *minGrowthBytes,
			MinElapsed:         *minElapsed,
			ArcInterval:        *arcInterval,
			NoBlink:            *noBlink,
			StageZoom:          *stageZoom,
			StageTile:          *stageTile,
			StageSock:          *stageSock,
			DryRun:             *dryRun,
			// Speak is deliberately left unset here — it's wired
			// per-session in serveWS, since each connected client must
			// hear its own replies through its own TTS connection, not
			// whichever client's orchestrator.Config happened to be
			// built. See server's doc comment for why this means one
			// orchestrator.Orchestrator per session, not a single shared
			// instance.
		},
	}

	mux, err := srv.routes()
	if err != nil {
		log.Fatalf("routes: %v", err)
	}

	addr := ":" + *port
	fmt.Fprintf(os.Stderr, "saturday-voice — listening on %s (STT=%s TTS=%s)\n", addr, *sttURL, *ttsURL)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// routes builds srv's HTTP handler — the static client at "/" and the
// audio WebSocket at "/ws". Factored out of main so tests can stand up a
// real HTTP server against it without going through flag parsing.
func (srv *server) routes() (http.Handler, error) {
	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("static fs: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticContent)))
	mux.HandleFunc("/ws", srv.serveWS)
	return mux, nil
}

// server holds saturday-voice's shared connection settings and the
// orchestrator.Config template each session builds its own Orchestrator
// from.
//
// Each connected client gets its own *orchestrator.Orchestrator instance
// (see serveWS), not a shared one — orchestrator.Config.Speak is set once
// at construction and has no per-call override, so a single shared
// instance would route every ask-mode reply and completion report through
// whichever client's TTS connection happened to be wired last. The cost:
// two simultaneous voice sessions don't share pending-inject state or the
// arc-summary cache — the same accepted-duplication tradeoff the plan
// already makes between saturday-mayor and saturday-voice, just at a
// finer grain. Fine for this pass; saturday-voice is meant to be
// single-user, not a multi-tenant service.
type server struct {
	orchTemplate orchestrator.Config

	authToken   string
	sttURL      string
	ttsURL      string
	moshiAPIKey string
	voice       moshiclient.TTSVoice
}

var upgrader = websocket.Upgrader{
	// The client and this server are meant to be on the same origin (this
	// binary serves both the static page and the WS endpoint) — same-origin
	// requests don't need CheckOrigin at all, but a same-origin default
	// still lets a browser tab loaded from elsewhere connect if it has the
	// token, which is fine given auth is enforced separately below.
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (srv *server) serveWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != srv.authToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	defer conn.Close()

	sess := newSession(nil, conn, srv.sttURL, srv.ttsURL, srv.moshiAPIKey, srv.voice)
	cfg := srv.orchTemplate
	cfg.Speak = sess.speak
	sess.orch = orchestrator.New(cfg)
	sess.orch.StartCompletionPolling()

	if err := sess.run(); err != nil {
		log.Printf("session ended: %v", err)
	}
}
