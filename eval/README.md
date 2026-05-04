# Saturday — Expander Backtest

First validation gate before any voice/audio work. Tests whether a small LLM,
given a user utterance + a snapshot of recent Claude Code session state, can
produce a prompt that lands the user's intent if injected into that session.

## What it does

1. Walks `~/.claude/projects/` and samples real user prompts from your JSONL
   transcripts (your typed prompts ≈ how you'd speak to Saturday).
2. For each sample, builds an "attentional state" from the events preceding
   that prompt (last user turn, last assistant text, last tool use + result,
   modified files, cwd).
3. Calls the **expander** (Haiku) with `(utterance, state)` →
   `{action, text, confidence, rationale}`.
4. Calls the **judge** (Sonnet) with `(real_user_prompt, state, expansion)` →
   `{grade, rationale}`.
5. Writes `results.csv` and prints a pass-rate / destructive-rate summary.

## Run

```bash
cp .env.example .env
$EDITOR .env                          # paste ANTHROPIC_API_KEY=sk-...

# first run on the synthetic public corpus (recommended for sharing results)
go run expander_backtest.go --corpus corpus.example.json

# or sample fresh from your own ~/.claude/projects (private, gitignored)
go run expander_backtest.go --refresh --sample 30
```

Re-runs are free (LLM responses are cached by content hash in `.cache/`).
Stdlib-only Go — no external deps.

## Corpora

- **`corpus.example.json`** — 30 synthetic, neutral samples committed to the
  repo. Use this when sharing results or running CI. Voice-utterance
  distribution: terse directives, soft-anaphoric references, parameter
  changes, status checks, conversational asides.
- **`corpus.json`** — pulled from your own `~/.claude/projects/` JSONL
  transcripts via `--refresh`. Gitignored. Stays local because it contains
  real cwds, project names, and prompt content from your sessions.

Use `--corpus PATH` to point at any frozen corpus. Pass `--refresh` to
overwrite (samples fresh and saves).

## Pass criteria

- `ship` + `needs_ask_correct` + `decline_correct` ≥ **90%**
- `destructive` < **2%**

If `pass_rate < 90%` or `destructive >= 2%`, the silent-injection model
doesn't ship — drop to a confirm-before-inject UX, or improve the expander
system prompt and re-run.

## Reading `results.csv`

```
column -t -s, < results.csv | less -S
```

Columns: `project, utterance, action, expansion, expander_confidence,
expander_rationale, grade, judge_rationale`.

## What this does NOT test

- Routing (which session) — separate test
- Whisper transcription accuracy on your vocabulary — separate test
- Reverse-expander (result → user-facing summary) — separate test
- TTS — separate test
- Live tool injection — only paper trail
