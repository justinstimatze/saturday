# deploy/modal

Modal deployment for `saturday-voice`'s `moshi-server` backend (STT + TTS,
Phase 1b hosting pivot — see `~/.claude/plans/wobbly-honking-valley.md`).
Runs the real Rust `moshi-server` binary, unmodified, so `moshiclient`'s
existing msgpack WebSocket client needs no changes regardless of whether
it's pointed at Runpod or Modal.

## One-time setup

```
modal secret create saturday-voice-moshi-auth MOSHI_AUTH_TOKEN=<a random token>
```

That token is what `moshi-server`'s `authorized_ids` gets rendered from at
container start, and what `saturday-voice --moshi-api-key` needs to match.

## Deploy

```
modal deploy deploy/modal/moshi_server.py
```

First deploy builds the image from scratch (Rust toolchain, `cargo install
--features cuda moshi-server@0.6.4`, `uv` for the TTS half's Python
component) — expect this to take a while and to need iteration; nothing
here has been live-verified against Modal yet, only ported from Unmute's
proven `dockerless/start_stt.sh`/`start_tts.sh` steps.

Prints two URLs, one per `@modal.web_server` method (`stt`, `tts`). Point
`saturday-voice` at them:

```
saturday-voice --moshi-stt-url wss://<stt-url>/api/asr-streaming \
                --moshi-tts-url wss://<tts-url>/api/tts_streaming \
                --moshi-api-key <the MOSHI_AUTH_TOKEN value>
```

## Logs / status

```
modal app logs saturday-voice-moshi
```

## Tear down

```
modal app stop saturday-voice-moshi
```

`scaledown_window=300` means it also scales to zero on its own after 5
minutes idle — unlike the Runpod pod this replaces, there's no ongoing
storage cost while stopped.

## Rotating the auth token

```
modal secret create saturday-voice-moshi-auth MOSHI_AUTH_TOKEN=<new token> --force
modal deploy deploy/modal/moshi_server.py
```
