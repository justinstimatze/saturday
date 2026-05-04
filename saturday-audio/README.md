# Saturday — Audio sidecar (V0.2.0)

Voice → text → mayor. Open-mic by default. Replaces mayor's stdin reader
with a Unix socket carrying line-delimited JSON utterances.

> **Important: `--hotphrase` is NOT a wake word.** Unlike Siri / Alexa /
> "Hey Google," Saturday's mic is always live — every utterance is heard,
> transcribed, and sent to mayor. The hotphrase (`"would you kindly"` by
> default) is a per-utterance **pipeline-mode switch**: prefixing it on
> an utterance flips that one utterance from VERBATIM mode (raw STT text
> goes through as the inject) to EXPAND mode (router + LLM-expander
> rewrites your phrasing into a project-grounded prompt + spoken
> narration). Mute via SPACEBAR in the sidecar pane is the actual "stop
> listening" gesture. See `--hotphrase` and `--mute-key` flags below.

## What it does

```
mic → Silero VAD → faster-whisper STT → JSON over Unix socket → saturday-mayor
```

- **Mic capture** at 16 kHz mono float32 via PortAudio (PipeWire on
  Ubuntu 25.10 just works through PortAudio's auto-detection).
- **Silero VAD** segments the stream into utterances. Default flush after
  700 ms of silence.
- **faster-whisper** `small.en` int8 transcribes flushed utterances. CPU
  only — adequate for an Intel laptop without a GPU. ~1 s STT for a 3 s
  utterance, runs while you're still speaking so perceived latency at
  end-of-utterance is closer to ~1.6 s once mayor's pipeline is added.
- **Mute toggle** — press SPACEBAR in the sidecar's terminal to flip
  `[live]` / `[muted]`. When muted, audio still flows through VAD (cheap)
  but transcripts are dropped before STT runs — neither logged nor sent.
- **Transcript log** — `~/.claude/saturday/transcripts/YYYY-MM-DD.log`,
  one line per heard utterance with ISO timestamp. Audit what was heard.
- **No-cache for muted utterances** — mute is a hard cutoff before STT
  even runs, so muted speech leaves no record anywhere.
- **Dynamic vocab bias** — every 60 s the sidecar pulls the watcher's
  session list and extracts "weird words" — project names, identifiers
  (`camelCase`, `snake_case`, `kebab-case`), filenames, and domain terms
  that recur in your recent user/assistant text — and feeds them to
  Whisper as `initial_prompt`. Without this, Whisper falls back to the
  closest English homophone (`adit` → `add it`, `lucida` → `lucid`,
  `stope` → `stop` / `stoped`). Pinned terms via `--vocab-pin` always
  take priority; useful for jargon you use across projects.
- **Spoken clarifier replies (V0.2.1)** — when mayor's expander returns
  `action="ask"` (low-confidence route or ambiguous expansion), mayor
  writes a `{"type":"speak","text":"..."}` event back over the audio
  socket. The sidecar synthesizes it via Kokoro TTS (24 kHz, default
  voice `af_heart`) and plays through the default output device. STT
  is auto-suppressed during playback (echo guard) — `…tts` is shown if
  VAD picks up speech in that window.

## Install

System dep (Ubuntu): PortAudio runtime.
```bash
sudo apt install libportaudio2
```

Python deps (recommend a venv):
```bash
cd saturday-audio
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
```

First run downloads `faster-whisper small.en int8` (~250 MB) to
`~/.cache/huggingface/`, the Silero VAD model (~2 MB) to torch hub
cache, and the Kokoro TTS model (~80 MB). Allow a few minutes the first
time; subsequent runs are instant.

## Run

Mayor must be running first with `--audio-sock` matching the sidecar's:

```bash
# terminal 1: watcher (existing)
saturday-watcher &

# terminal 2: mayor in audio-socket mode
saturday-mayor --audio-sock /tmp/saturday-audio.sock

# terminal 3: audio sidecar (this binary)
cd saturday-audio && source .venv/bin/activate
python main.py
```

Then talk. SPACEBAR toggles mute, `q` quits.

If the sidecar starts before mayor has the socket open, it retries every
2 s. No special start order is required.

## Wire format

Line-delimited JSON. The sidecar writes; mayor reads:

```json
{"type":"utterance","text":"in lucida, run git status","ts":1714780123.45}
```

Mayor ignores any `type` it doesn't know, so future event types
(e.g. `partial`, `cancel`) can be added without breaking compatibility.

## Flags

| flag             | default                                  | meaning                                        |
|------------------|------------------------------------------|------------------------------------------------|
| `--audio-sock`   | `/tmp/saturday-audio.sock`               | Unix socket to write to (must match mayor)     |
| `--model`        | `small.en`                               | faster-whisper model. Try `base.en` if too slow on your CPU; `medium.en` if you have headroom and want better WER |
| `--hotphrase`    | `would you kindly`                       | **Pipeline-mode switch, NOT a wake word.** Mic stays live regardless. Prefix this on an utterance to opt that one utterance into EXPAND mode (router + LLM-rewrite + narration); without it, utterance is VERBATIM (raw STT text injected as-is). Set to empty string to put every utterance in expand mode. |
| `--mute-key`     | `<space>`                                | terminal key to toggle mute (default spacebar). This IS the "stop listening" control — when muted, transcripts are dropped before STT runs (no record). |
| `--quit-key`     | `q`                                      | terminal key to quit                           |
| `--transcripts`  | `~/.claude/saturday/transcripts/`        | daily transcript log dir                       |
| `--mode`         | `open`                                   | `open` = always-on (default). `ptt` reserved for V0.2.1 |
| `--watcher-sock` | `/tmp/saturday-watcher.sock`             | watcher socket — sidecar pulls vocab from `/state` here |
| `--vocab-pin`    | (empty)                                  | comma-separated terms to ALWAYS bias toward, on top of dynamic pool. Use for jargon (`stope,kokoro,silero`) that may not appear in any active session yet |
| `--vocab-refresh-sec` | `60`                                | how often to re-pull dynamic vocab from watcher |
| `--tts-voice`    | `bm_george`                              | Kokoro voice ID. Single voice: `af_sarah`, `am_adam`, `bm_lewis`, etc. **Blending**: `am_adam+af_sarah` averages two voice embeddings — neg. perf cost. **Funny-leaning** picks: `bm_fable` (theatrical british), `bm_lewis` (nasal-comic), `am_puck` (off-kilter), `am_eric` (nerdy), `af_nicole` (quirky). Set `HF_TOKEN` env var if you want to skip the unauthenticated-request warning on first download. |

## Latency tuning

If end-of-utterance feels too slow:
- `--model base.en` (faster, lower WER on accents — usually fine for command-style input)
- Lower `SILENCE_FLUSH_MS` in `main.py` from 700 to 500 (more aggressive flush, more clips on slow speakers)

If you see false flushes mid-sentence:
- Raise `SILENCE_FLUSH_MS` to 900-1000.

If STT mis-transcribes commands:
- `--model medium.en` (slower, better WER). Or revisit V0.2 roadmap for
  domain-specific prompt biasing (faster-whisper supports `initial_prompt`).

## Privacy notes

Open-mic continuously listens. Transcripts persist in plaintext under
`~/.claude/saturday/transcripts/`. Mute (`m`) is the hard cutoff — it
short-circuits before STT and logging. Voice command `saturday mute`
deferred to V0.2.1 (would need an inline keyword pre-filter in the
sidecar before the routing pipeline sees it).

Mayor's LLM cache (`saturday-mayor/.cache/`) is keyed by content hash so
random transcribed utterances accumulate as separate cache entries — fine
operationally but worth knowing if disk usage matters or you want a clean
slate.
