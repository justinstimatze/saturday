# Saturday — Sync Hook

`UserPromptSubmit` hook for Claude Code. Detects out-of-band turns in
the session JSONL (Saturday's headless `claude --resume --print`
injects) and emits an `<earlier-turn channel="voice">` context block so
the live pane sees them on the user's next interaction.

Closes the in-memory/disk divergence that the headless injection path
opens up. See `../INJECTION.md` for the full rationale and probe data.

## Build

```bash
make install        # from the workspace root → $(go env GOPATH)/bin/saturday-sync
```

## Install

CC's hook runner needs an absolute path. Substitute the output of
`go env GOPATH`:

```bash
jq --arg sync "$(go env GOPATH)/bin/saturday-sync" \
  '.hooks.UserPromptSubmit += [{"matcher":"","hooks":[{"type":"command","command":$sync}]}]' \
  ~/.claude/settings.json > /tmp/s.new && mv /tmp/s.new ~/.claude/settings.json
```

Fires on every `UserPromptSubmit` across every session. Sessions that
have never received a Saturday inject just see a no-op (cursor primes,
no output). Cost per fire: one JSONL read + one cursor file write.

**Already-running sessions need to be restarted to pick up the new hook.**
Claude Code reads `settings.json` once at session start. After the
initial install the binary path is registered; rebuilding the binary
afterwards doesn't need a restart, since the binary is invoked fresh
on every fire.

## Headless detection

`claude --resume --print` (Saturday's own headless injector) also fires
`UserPromptSubmit` hooks. Without a guard, the sync hook would treat
the inject's prompt text as a "live submission" and filter the inject
turn out of the next live-pane fire's report. Fix: walk
`/proc/<ppid>/cmdline` up the process tree (Claude Code wraps the hook
in `/bin/sh -c`, so claude is at depth ≥1) and look for `--print`/`-p`.
If present, exit immediately — no cursor advance, no output. See
`isHeadless()` in `main.go`.

## State

Per-session cursor at `~/.claude/saturday/cursors/<session_id>.json`:

```json
{ "timestamp": "<latest user-turn ts at last fire>",
  "last_live_prompts": ["<recent live submissions, oldest-first, capped at 5>"] }
```

Each fire appends `in.Prompt` to the ring buffer and drops the oldest
entry past the cap. Pairs whose user text matches *any* element are
filtered out of the voice-channel report. A single-string cursor
(`last_live_prompt: "..."`) reads cleanly and is migrated to a
one-element array on next save — no manual migration needed.

The buffer is what catches bursts: if the user fires P1 then P2 in
quick succession before the next out-of-band turn lands, the legacy
single-prompt cursor would only filter P2 and leak P1 as voice context
on the third fire. Five entries covers realistic burst sizes without
unbounded growth.

## Output framing — frozen template

```
<earlier-turn channel="voice">
Saturday relayed these voice exchanges to this session while you were at the prompt:
  voice request: <utterance>
  auto reply: <reply, truncated to 300 chars>
  (no auto reply — request still pending; please act if it hasn't been satisfied above)
These exchanges aren't in the terminal scrollback.
</earlier-turn>
```

Don't drift on the wrapper (lines 1, 2, 6, 7 above). They're part of
the cached 1h prompt prefix for every ongoing session. The per-pair
inner lines (`voice request:`, `auto reply:`, `(no auto reply ...)`)
can be iterated — that's where the directive-vs-context shaping lives.

Each emitted exchange shows one of two shapes:
- `voice request:` followed by `auto reply:` — Saturday's headless
  inject produced a response, which is in JSONL. The live pane should
  treat the auto-reply as informational (it may be wrong if autocompact
  diverted it) and verify whether the request was actually satisfied.
- `voice request:` followed by `(no auto reply — request still pending)`
  — direct-write path (see ROADMAP / mayor's `--inject-direct-threshold`).
  The user turn was written to JSONL without invoking headless claude;
  the live pane is the only handler. Acting on it is mandatory.

## Pairing logic — semantic, not temporal

For each user turn, the assistant reply is the first assistant turn
(with non-empty text content) found by BFS through descendants via the
`parentUuid` chain. Tool-use chains aren't summarized — only the text
portion is shown. (V1.1 problem: summarize tool-use chains semantically.)

**Why descendants, not temporal adjacency:** when mayor's direct-write
path appends a user turn while CC is mid-session, that turn is orphan
in the live conversation tree — CC tracks its leaf in-memory and
doesn't re-read JSONL between turns. Pairing the orphan with the next
temporally adjacent assistant text would mis-attribute a reply that's
actually answering a different prompt, and the framing would mislead
the live claude into believing the voice request was already handled.
Walking parent→child links makes pairing semantic. Orphans correctly
fall out as `(no auto reply — request still pending)`, cuing the live
pane to act.

## Filtered turn types

These user turns are skipped — they're never voice-channel candidates and
emitting them as `<earlier-turn channel="voice">` is a false positive:

- `isMeta: true` — Claude Code's caveat blocks (e.g. `<local-command-caveat>`)
- `isCompactSummary: true` — the post-`/compact` summary block
- content starts with `<command-name>` — typed slash commands (e.g. `/compact`)
- content starts with `<local-command-stdout>` — slash command stdout

The first two are flags Claude Code sets on its own meta-turns. The latter
two are content-prefix discriminators for slash-command framing. Together
these handle all observed `/compact`-related noise.

Philosophy: filter, don't gate. A confirmation prompt the user has to
clear on every utterance becomes friction theater long before it catches
anything real.

## Fail-closed

Any error path → exit 0 with no output. Hooks must not disrupt the
live pane. Bad input, missing JSONL, JSON parse failure: silent return.

## Uninstall

```bash
jq '.hooks.UserPromptSubmit |= map(select(.hooks[0].command != "/home/<user>/go/bin/saturday-sync"))' ~/.claude/settings.json > /tmp/s.new && mv /tmp/s.new ~/.claude/settings.json
rm -rf ~/.claude/saturday/cursors
```
