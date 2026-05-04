# Saturday — Injection Layer

How V0.1 puts an expanded utterance into the target Claude Code session.

## Decision

**Inject via `claude --resume <sid> --print '<text>'`.**
No tmux, no keystroke injection, no terminal-emulator coupling.

A `UserPromptSubmit` sync hook on each session reconstitutes out-of-band
inject context for the live pane on the user's next interaction.

## Why not tmux / keystroke injection

Considered and rejected:

| approach            | verdict | reason                                                          |
|---------------------|---------|-----------------------------------------------------------------|
| `tmux send-keys`    | reject  | requires multiplexer adoption; user is on bare terminator       |
| TIOCSTI ioctl       | dead    | locked behind `dev.tty.legacy_tiocsti=0` since kernel 6.2       |
| `xdotool`           | dead    | X11-only; user is on Wayland (Ubuntu 25.10)                     |
| `ydotool` (uinput)  | reject  | works on Wayland but fragile: focus-dependent, needs daemon     |
| emulator sockets    | reject  | terminator is VTE-based with no remote-control protocol; the    |
|                     |         | only emulators with sockets (kitty, wezterm, foot) would couple |
|                     |         | Saturday to a single emulator                                   |
| `claude --resume`   | accept  | works headless, terminal-agnostic, no extra processes           |

The "system-native" answer for modern Wayland Linux is ydotool, but the
focus-fragility (must focus the right terminal at inject time) is worse
than tmux for this use case. The headless path sidesteps the entire
keystroke-layer problem.

## Probe evidence

Probe results below are anchored to a single live run against Claude
Code 2.1.x; the wall-clock and dollar figures will drift with model
pricing, CLAUDE.md size, and CC's internal cache strategy. The
*structural* findings (cache reuse, JSONL coherence, the live-pane
visibility gap) are what's load-bearing.

- **Phase 1 (cold session creation, fresh sid):**
  exit=0, ~9s wall, ~1.3s API. The bulk of input tokens are CLAUDE.md
  auto-load — sunk cost per fresh session, not per inject.

- **Phase 2 (resume + print):**
  exit=0, ~1.3s wall, near-prefix-cache rate. Model correctly recalled
  the "42" from phase 1 across processes. Confirms `--resume`
  reconstructs the conversation from JSONL and the API has full prior
  context.

- **Phase 3 (concurrent --print to same sid):**
  Both succeeded individually (`A` and `B` returned correctly). No
  lockfile error. **But JSONL got interleaved** (user/assistant turns from
  A and B mixed). Constraint: Saturday must serialize injects per
  session_id. Trivial to enforce — mayor holds a per-session lock,
  one inject at a time.

- **Live-pane test (interactive claude held in terminator + headless
  inject from another terminal):**
  Headless inject succeeded (`PINEAPPLE` returned, JSONL went 13→26 lines
  cleanly). Live pane displayed nothing. When user typed in live pane
  *"what was the last thing I asked you?"*, response was *"you said this
  is just a test"* — the inject was invisible to the live process. This
  is outcome (b): live process operates from stale in-memory transcript,
  doesn't re-read JSONL on each turn.

The stale-in-memory problem is the load-bearing finding. The fix below
addresses it.

## Sync hook — UserPromptSubmit

For each session that participates in Saturday, install a `UserPromptSubmit`
hook that runs on every user prompt in that session.

**Mechanism:**
1. Hook receives `session_id` in its input.
2. Read the session JSONL.
3. Compare turn count against a sidecar cursor at
   `~/.claude/saturday/cursors/<sid>` (a single integer).
4. If JSONL has more turns than the cursor, the delta turns are
   Saturday-injected (or any other out-of-band writer). For each delta
   user/assistant pair, append a context block to the user's prompt:

   ```
   <earlier-turn channel="voice">
   The user spoke this via voice (transcribed by Saturday and routed here):
     user: <inject_user verbatim, short>
     you: <inject_assistant summary, 1 line>
   This exchange ran headless and isn't in the terminal scrollback.
   </earlier-turn>
   ```

5. Update cursor to current turn count.

**Framing rationale:**
- Frame as user-via-voice, not as "Saturday says X". The inject's user
  turn IS the user's intent (Saturday transcribed and expanded; the source
  is the user's voice). The inject's assistant turn IS this Claude
  (same session, just a different process). Naming Saturday once as the
  transport is honest and gives the model a referent if asked, without
  elevating Saturday to "agent that talks."
- Less prompt-injection-attack-shaped. `<saturday says>` looks like an
  injection vector. `<earlier-turn channel="voice">` looks like the
  conversation history it actually is.
- No behavioral instructions (no "treat this as if the user typed it").
  Start bare. Add guardrails only if the live pane starts narrating
  Saturday's involvement when not asked.

**Frozen template, cache discipline:**
- The XML tag, attributes, and surrounding framing must not drift across
  versions. Once written, it becomes part of the cached 1h prefix for
  every ongoing session — changing it invalidates all of them.
- Hook output must be byte-deterministic for the same JSONL state. No
  timestamps in the framing, no random ordering, sorted turn data.

## Caching strategy

Claude Code already requests 1h cache TTL (confirmed in probe: response
contained `ephemeral_1h_input_tokens`). Working with that.

**Per-turn math:**
- Note size ~100 tokens for a single inject summary.
- Cache-creation cost: 2× normal input rate (1h cache writes are 2×; 5min
  is 1.25×).
- Cache-read cost: ~10% normal input rate.
- Each note pays full price once at insertion, then ~10 tokens/turn on
  cache reads. Break-even after ~1 turn; near-free thereafter as long as
  the next live-pane interaction is within 1h.

**Natural inject pattern hits warm cache:**
- User speaks → router → expander → headless inject. The inject
  populates the session's prefix cache.
- User typically engages with the live pane within seconds (because they
  just spoke about it). Live pane's next turn lands within the 1h window.
- Cold-cache cost only applies to "inject and walk away >1h" — rare for
  active engagement.

**Periodic no-op ping for kept-warm sessions:**
For sessions Saturday actively cares about (currently ambient in the
user's awareness), schedule a no-op `claude --resume <sid> --print 'ping'`
every ~55 minutes. Costs ~10% of prefix-token rate (cache-read) per ping.
Keeps the 1h cache hot indefinitely. The active-window filter (or, later,
[drivermap](https://github.com/justinstimatze/drivermap) as a richer
attentional source) decides which sessions qualify — wasteful to ping
every session when only a handful are active.

(Not in V0.1 — V0.1 just accepts cold-cache penalty on the rare
walkaway. Implement if measured cost justifies it.)

**ToS posture for the no-op ping:** legitimate use. Each ping is a real
billed API call against the user's own quota; prompt cache is documented
as a paid feature for repeated-prefix workflows; keep-warm patterns are
standard infra (AWS Lambda warmup, RDS connection pings). To stay
clearly on the right side: ping cadence ≥ 55 min, only for sessions the
active-window filter considers current, never automated against sessions
the user has abandoned. Anti-pattern to avoid: sub-minute pings or
pinging all sessions blindly — that's quota-padding-shaped even if not
intentional.

## Token cost mitigations

Beyond caching, the note text itself can compound when many injects
stack between live-pane interactions. Mitigations in increasing effort:

1. **Summarize the inject's assistant turn** (V0.1).
   The inject's user turn is short voice transcript — quote verbatim, cheap.
   The reply is the bloat (could be a 500-line code-edit response).
   Saturday's mayor already knows what the inject was about; have it
   write a 1-line summary at inject time and inline that, not the full reply.

2. **TTL on notes** (V1.1).
   Hook only re-injects context for injects newer than ~10 min. Older
   injects are assumed integrated via earlier turns. If user references
   something from yesterday, that's a real recall problem and they'll ask
   explicitly.

3. **On-demand retrieval via tool** (V1.1).
   Inline becomes a 1-line breadcrumb (`Saturday relayed 3 voice exchanges
   in the last 20 min`); live Claude calls a `saturday_recall(turn_id)`
   tool to fetch full content if user references it. Pay tokens only on
   retrieval. Heavier to build but bounded ambient cost.

   Note: this slightly contradicts Saturday's "hooks > MCP" rule. That
   rule is for *push-injection* (must-see context). Pull-retrieval where
   the model decides whether to fetch detail is a different problem
   and tool-use fits natively.

(2) and (3) compose. (1) is free, build into the hook from day one.

## V0.1 component shape after these decisions

| component         | what it is                                       |
|-------------------|--------------------------------------------------|
| injector          | one-liner: `claude --resume <sid> --print '<text>'` |
| sync hook         | UserPromptSubmit JSONL-cursor diff + note inject |
| saturday-mayor    | orchestrates watcher → router → expander → inject |

The keystroke-injection binary that V0.1 originally listed dissolves to
nothing. The sync hook is the new V0.1 component the headless approach
introduces.

**Mediator dropped from V0.1.** Originally specced as a ~1s text-mode
abort gate before each inject. Cut on review: the window is too short
to be a real abort (perceive + decide + key-press exceeds 1s), and the
false-positive risk it was hedging against — Saturday firing on a
misheard transcript — only exists in V0.2 audio mode. In V0.1 the user
authors every utterance via stdin; there's nothing to abort against.
Mayor execs `claude --resume --print` directly and prints a one-line
proposal log to stderr so the user sees what fired. If V0.2 surfaces
ambiguous transcripts, a real confirmation mechanism (press-to-commit,
not press-to-abort) returns then.

## Constraints to respect

- **Serialize injects per session_id.** JSONL appends from concurrent
  `--resume --print` processes interleave. Mayor enforces this — one
  in-flight inject per session at a time.
- **Don't drift the `<earlier-turn>` framing.** Frozen template = cached
  prefix integrity across sessions and time.
- **Hook output deterministic.** Same JSONL state → byte-identical note.
  No timestamps, no UUIDs in the framing, sorted turn data.
- **Cursor file is per-session.** `~/.claude/saturday/cursors/<sid>` —
  one integer (turn count). Hook reads, compares, writes. Atomic via
  temp-file-then-rename if multiple hook invocations could race (they
  shouldn't — UserPromptSubmit is per-turn-per-session).

## Gotchas discovered during V0.1 implementation

- **`claude --resume --print` also fires UserPromptSubmit hooks.** Saturday's
  own headless inject triggers the sync hook, which would otherwise treat
  the inject's text as the user's "previous live prompt" and filter it out
  on the next live-pane fire. Fix: walk `/proc/<ppid>/cmdline` up the tree
  (Claude Code wraps hook commands in `/bin/sh -c`, so claude is at depth
  ≥1) looking for `--print`/`-p`. If found, exit silently — no cursor
  update, no output. See `sync/main.go::isHeadless`.
- **Hooks added to `settings.json` mid-session don't apply to that
  running session.** Restart required. Modifying an already-installed
  hook's binary is fine — it's invoked fresh each fire.
- **Hooks producing zero output don't appear in JSONL `hook_success`
  attachments.** Made debugging "did my hook even fire?" harder. The
  cursor file's mtime is a more reliable proof-of-fire signal.
- **The user's current prompt is NOT in JSONL at hook fire time.** Hook
  input has it in the `prompt` field. The previous live submission *is*
  in JSONL by next fire — that's what `last_live_prompt` filters out.

## Open questions

- **Hook latency budget.** UserPromptSubmit runs synchronously before the
  prompt goes to the API. JSONL read + cursor diff should be <50ms even
  on large sessions. Probe under realistic load before committing.
- **What if the live pane is mid-turn when Saturday injects?** Mayor
  serializes the user-side via one-inject-per-session, but if the user
  is typing when the inject lands, the live pane will append turns
  after the inject's turns in the JSONL. Should still work — JSONL is
  linear, the hook will pick up the inject on the *next* user
  submission. But worth verifying the live pane's internal state stays
  coherent.
- **Many sessions × many users of this pattern.** Each session pays its
  own 1h cache. Currently fine for one user with 6-8 sessions; would need
  rethink at scale.
