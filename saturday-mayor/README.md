# Saturday — Mayor

V0.1 silent-loop orchestrator. Reads utterances line-by-line from stdin (V0.1
STT placeholder), routes them to the right active Claude Code session, expands
them into project-grounded prompts, and exec's `claude --resume <sid> --print`
headless to inject.

V0.2 will swap stdin for an audio sidecar.

## Build

```bash
cd saturday-mayor && go build -o ~/go/bin/saturday-mayor .
```

## Setup

Mayor reads `ANTHROPIC_API_KEY` from a `.env`-format file. Default search
order:

1. `--env <path>` (CLI override)
2. `$XDG_CONFIG_HOME/saturday/config` — XDG-standard location, recommended.
   Defaults to `~/.config/saturday/config` when `XDG_CONFIG_HOME` is unset.
3. `<scriptDir>/.env` — falls back to legacy in-repo location for dev.
   When mayor is installed at `~/go/bin/saturday-mayor` (no `main.go` in
   `~/go/bin/`), `scriptDir` resolves to the current working directory.

To install once and forget:

```bash
mkdir -p ~/.config/saturday
echo 'ANTHROPIC_API_KEY=sk-...' > ~/.config/saturday/config
chmod 600 ~/.config/saturday/config
```

`saturday-stack` auto-migrates a legacy `saturday-mayor/.env` into the
XDG location on first run.

The LLM-response cache also moved to XDG: defaults to
`$XDG_CACHE_HOME/saturday/llm/` (i.e. `~/.cache/saturday/llm/`) so
gitignored cache no longer pollutes the repo. Override with `--cache`.

## Run

`saturday-watcher` must be running first — mayor queries it for live session
state on every utterance:

```bash
~/go/bin/saturday-watcher &                          # in one terminal
~/go/bin/saturday-mayor                              # in another
~/go/bin/saturday-mayor --dry-run                    # log proposals, don't inject
```

Then type utterances, one per line. Each line goes through the pipeline:

```
stdin → watcher /state → router (Haiku) → expander (Haiku) → claude --resume --print
```

Output (stderr):
```
→ route: lucida (conf=0.92) — <one-line rationale>
→ Saturday → lucida (conf=0.81): <expanded prompt>
  ↳ injected, 412 bytes assistant reply
```

## Pipeline behavior

- **One active session**: skip the router, expand against the only target.
- **Multiple active sessions**: router picks one. If router `confidence`
  falls below `--conf-threshold` (default 0.5), mayor logs the would-be
  route to stderr and skips the inject — the user sees the proposal but
  the live pane is undisturbed. Set `--conf-threshold 0` to disable the
  gate and always inject.
- **Expander action="inject"**: mayor picks a path in priority order
  (see "Inject path selection" below). Same `--conf-threshold` gate
  applies to expander confidence; below threshold the proposal logs
  and skips.

## Inject path selection (V0.1.5 primary → fallbacks)

For each accepted inject, mayor walks paths in this order:

1. **tmux send-keys (preferred).** Mayor walks `tmux list-panes -aF
   '#{pane_id} #{pane_pid}'`, descends each pane's process tree via
   `/proc/<pid>/task/<pid>/children`, finds claude processes, matches
   on cwd. If a pane hosts a claude in the target session's cwd, mayor
   types the expanded prompt + Enter into that pane via two
   `tmux send-keys` calls (`-l <text>` then `Enter`). The live claude
   handles it natively: full scrollback, permissions inherit, no
   autocompact-divert risk, no orphan turns. Requires the target
   session's claude to be running inside tmux — see
   `bin/saturday-claude`.
2. **Direct-write to JSONL (fallback for big non-tmux sessions).** If
   no tmux pane hosts the target's claude AND the target JSONL has more
   than `--inject-direct-threshold` (default 80k) tokens since the most
   recent `isCompactSummary`, mayor appends a synthetic user turn
   directly to the JSONL under `flock`, with full schema chained off the
   latest `last-prompt`'s `leafUuid`. Sync hook surfaces it on the
   user's next interaction in that pane as `(no auto reply — request
   still pending)`, cueing the live claude to act. Loses
   autonomous-background-work but avoids autocompact-divert.
3. **Headless `claude --resume <sid> --print '<text>'` (fallback for
   small non-tmux sessions).** Spawns a headless claude turn that
   actually runs the work and writes the assistant reply back to JSONL.
   Autonomous-background-work for sessions small enough not to trigger
   autocompact-on-load.

The recommended setup: run `claude` via `saturday-claude` (or otherwise
inside tmux), so path 1 always applies. Paths 2 and 3 are graceful
degradation for sessions started without tmux.
- **Expander action="ask"**: log the clarifier question to stderr, no inject.
  V0.1 doesn't surface this back to the user via voice — they see it in mayor's
  log. V0.2 wires this through the audio sidecar.
- **Expander action="decline"**: log and skip.

## Sequential, single-flight

Mayor processes one stdin line at a time. JSONL-write serialization is
implicit between mayor's own injects (no two `claude --resume --print` from
mayor in flight at once).

## Collision avoidance with the user's live CC

If the user is actively typing into a target CC session at the same moment
mayor injects, the two `claude --resume` processes interleave their JSONL
writes (see `INJECTION.md` Phase 3 probe). V0.1.1 mitigation: before
`exec`'ing the inject, mayor stats the target's JSONL and waits for the size
to be stable for `--collision-wait` (default 500ms), giving up after
`--collision-max` (default 5s) and injecting anyway. Catches "user is mid-turn
right now" cases — assistant streaming and tool chains keep JSONL size
changing. Doesn't help if the user starts typing during the brief window
between probe and exec; that's V0.2 lockfile territory (see ROADMAP).

## Prompt source-of-truth

Router and expander prompts, tools, and the cache plumbing live in
`saturday/llmcore`. Mayor, `eval/router`, and `eval/expander_backtest`
all import from there. The eval pass-rates (93% router @ 4 candidates,
~50% expander on the local corpus, 0% destructive) reflect the current
prompts in `llmcore`.

## Cache

LLM responses cached to `.cache/<sha256-prefix>.json` keyed by content hash.
Same scheme as eval (different dir to avoid cross-contamination). Re-running
the same utterance against the same session state is free.

## Flags

| flag                | default                      | meaning                                  |
|---------------------|------------------------------|------------------------------------------|
| `--sock`            | `/tmp/saturday-watcher.sock` | watcher Unix socket                      |
| `--env`             | `./.env`                     | file containing `ANTHROPIC_API_KEY`      |
| `--cache`           | `./.cache`                   | LLM response cache dir                   |
| `--dry-run`         | false                        | log proposals, don't exec claude         |
| `--collision-wait`  | `500ms`                      | JSONL must be stable this long pre-inject |
| `--collision-max`   | `5s`                         | give up waiting and inject anyway        |
| `--conf-threshold`  | `0.5`                        | skip inject if router/expander conf < this; 0 disables |
| `--inject-direct-threshold` | `80000`              | if target JSONL has more than this many tokens since the last `isCompactSummary`, skip `claude --resume --print` and write the user turn directly to JSONL (sync hook + live pane handles it); 0 disables direct-write entirely. 80k is conservative; lower if you see autocompact-divert symptoms |
