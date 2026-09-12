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
component) — expect this to take a while and to need iteration. This path
has been live-verified end to end against `saturday-voice` (real bugs found
and fixed via a live browser test, not just a clean deploy), though it can
sit untouched for weeks between sessions — expect to shake out small drift
(base image, pinned versions) on the first redeploy after a long gap.

## Modal vs. a rented Runpod Pod

`saturday-voice`'s Phase 0 validation spike deliberately used a rented
Runpod Pod instead of this Modal path — Runpod ran Unmute's Docker Compose
stack (later, its Dockerless mode, since Compose itself isn't supported on
Runpod Pods) with zero decomposition work, which fit a one-off spike better
than splitting the STT/LLM/TTS services into separate Modal Functions
up front.

That tradeoff inverts once past the one-off-spike stage:

- **Usage shape.** A rented pod bills for every second it's up, including
  silence between calls — fine for an hour of validation, a real cost for
  ongoing, bursty use (voice sessions in streaks, long idle gaps between).
  Modal's per-second billing + scale-to-zero (`scaledown_window` in
  `moshi_server.py`) charges only for time actually in use, no
  start/stop management needed.
- **Availability risk.** A rented Secure Cloud pod is pinned to whichever
  physical host it was created on — if that host runs out of free GPUs,
  the pod can't restart until capacity frees up there specifically, with
  no ETA and no portable-storage fallback unless a Network Volume was
  attached up front (a Pod's own local disk isn't one). Confirmed live:
  a stopped pod refused to restart on exactly this "not enough free GPUs
  on the host machine" error, while the same GPU type showed real stock
  on Modal-style serverless allocation the whole time. Modal's model has
  no equivalent "this specific box is full" failure mode.
- **Storage cost while idle.** A stopped Runpod pod's container-disk
  storage *doubles* in price while stopped rather than running, unless
  weights are deliberately moved to Runpod's separate network-storage
  tier — an easy-to-miss gotcha, not the "free while stopped" story it
  sounds like. Modal has no idle-storage cost at all when scaled to zero.

Net: Runpod was the right call for a single validation spike, and Modal is
the right call for anything past that — which is exactly why this file
exists. If Modal ever becomes the wrong call again (e.g. this exact
multi-service build turns out to need capabilities Modal genuinely can't
give it), that's a decision worth re-deriving explicitly rather than
drifting back to whichever one is already warm out of habit.

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
