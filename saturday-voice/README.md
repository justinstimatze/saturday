# Saturday — Voice

Talks to Kyutai's `moshi-server` STT/TTS directly over their native
msgpack/WebSocket protocol (see `saturday/moshiclient`) and drives
Saturday's own ask/route/inject/summarize decision core (see
`saturday/orchestrator` — the same one `saturday-mayor` runs for its
local-mic path). No Unmute backend or frontend in this build; Unmute was
Phase 0's validation harness only, not a runtime dependency.

Serves a minimal static test client (`getUserMedia` capture, WebSocket
audio streaming, Web Audio playback) at `/`, and the client-facing audio
WebSocket at `/ws`. Not the real Saturday-branded client — that's
roadmap; this one exists to drive a real end-to-end test.

## What this needs that `saturday-mayor` doesn't

`moshi-server`'s STT/TTS need a GPU. This machine may not have a discrete
one — check with `lspci`. Either way, `saturday-voice` itself has no GPU
dependency; it's a thin Go client that expects `moshi-server`'s STT and
TTS to already be running *somewhere* reachable over HTTP(S)/WS(S) —
typically a rented pod (Runpod, Modal, etc). Point `--moshi-stt-url` and
`--moshi-tts-url` at wherever that is.

## Build

From the workspace root:

```bash
make install        # → $(go env GOPATH)/bin/saturday-voice (and the others)
```

## Run

```bash
saturday-voice \
  --auth-token "$(openssl rand -hex 16)" \
  --moshi-stt-url https://<pod>.proxy.runpod.net/stt-raw \
  --moshi-tts-url https://<pod>.proxy.runpod.net/tts-raw \
  --port 8080
```

Then open `http://localhost:8080/?token=<the same token>` in a browser and
click Connect. `--auth-token` is mandatory, not optional — this endpoint
can act on a live coding session. Browsers can't set custom WebSocket
headers, so the token travels as a `?token=` query parameter on the `/ws`
connection rather than an `Authorization` header.

Shares `saturday-mayor`'s `.env`/cache conventions (`--env`, `--cache`,
`ANTHROPIC_API_KEY`) — nothing extra to configure if mayor already runs
here.

## Flags

Run `saturday-voice --help` for the full, current list — the orchestrator
knobs (`--conf-threshold`, `--ask-conf`, `--stability-window`, etc.) mirror
`saturday-mayor`'s own flags exactly, since both drive the same
`orchestrator.Orchestrator`. Voice-specific: `--auth-token`,
`--moshi-stt-url`, `--moshi-tts-url`, `--moshi-api-key` (moshi-server's
`kyutai-api-key` header, if it requires one), `--voice` (TTS voice id),
`--port`.

## Known limitations, not silently papered over

- **Single-client-at-a-time.** Each connected client gets its own
  `orchestrator.Orchestrator` instance (see `main.go`'s doc comment on
  `server`) so ask-mode replies and completion reports route to the right
  client's TTS — but that means two simultaneous voice sessions don't
  share pending-inject state or the arc-summary cache. Fine for a
  single-user assistant; would need real per-session routing for anything
  more.
- **A barge-in can't cancel an in-flight `orchestrator.Handle` call** — it
  has no cancellation support (see `orchestrator`'s own doc comment; it's
  deliberately synchronous, matching `saturday-mayor`'s existing
  behavior). What actually happens on interrupt: the current turn's TTS
  connection is closed immediately (stops audio), but `Handle` keeps
  running in the background and its eventual result is just discarded
  once superseded — not aborted.
- **STT reconnect resets the turn, it doesn't resume it.** A dropped STT
  connection (the likely failure mode on a WAN-connected, GPU-contended
  rented pod — Phase 0.5 hit exactly this once) reconnects with capped
  exponential backoff, but whatever the user was mid-utterance saying is
  lost; they'll need to repeat it. A seamless resume wasn't attempted this
  pass.
- **Backend-leg transport security relies on whatever's already true of
  the URL you point `--moshi-stt-url`/`--moshi-tts-url` at** — an
  `https://`/`wss://` URL through a rented pod's own TLS-terminating proxy
  (e.g. Runpod's `*.proxy.runpod.net`) is encrypted in transit for free;
  `--moshi-api-key` is wired through to moshi-server's `kyutai-api-key`
  header if you need an actual credential check, but whether moshi-server
  enforces it depends on its own launch config. Not independently
  hardened beyond that in this pass.

Full architecture and the reasoning behind these tradeoffs live in the
implementation plan this was built from — ask if you need it surfaced
again, it isn't tracked in this repo.
