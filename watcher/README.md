# Saturday — Live Session Watcher

A Go binary that polls `~/.claude/projects/<encoded>/<uuid>.jsonl` files
at sub-second cadence, maintains an in-memory "attentional state"
snapshot per active session, and serves it over a Unix socket so
downstream consumers (router, expander, mediator) can read current
state in microseconds.

Stdlib-only Go, single binary, no external deps.

## Run

```bash
cd watcher
go run main.go                            # ~/.claude/projects, 200ms poll, 30m active window
go run main.go --interval 100ms --active 1h --replay 20
```

## Read state from another terminal

```bash
# all active sessions, sorted by recency
curl --unix-socket /tmp/saturday-watcher.sock http://x/state | jq

# one session by project name or session_id
curl --unix-socket /tmp/saturday-watcher.sock http://x/state/lucida | jq

# filter by project on the list endpoint
curl --unix-socket /tmp/saturday-watcher.sock 'http://x/state?project=groupchat' | jq
```

## State shape

Same as `eval/expander_backtest.go`'s `State` type — wire-compatible
with the expander backtest harness so the router and expander can
consume watcher output directly.

```json
{
  "state": {
    "project": "lucida",
    "session_id": "...",
    "cwd": "/home/.../lucida",
    "last_user_turn": "...",
    "last_assistant_text": "...",
    "last_tool_use": {"name": "Edit", "input_summary": "..."},
    "last_tool_result_tail": "...",
    "modified_files": ["..."]
  },
  "last_event_at": "2026-05-02T22:01:30Z",
  "jsonl_path": "...",
  "events_seen": 47
}
```

## Design notes

The project-name decoding and active-window filter (30 min default) are
lifted from an earlier session-watcher experiment ([lucida](https://github.com/justinstimatze/lucida))
that ran LLM segmentation in the hot loop and persisted per-file state to
disk. Saturday's watcher stays stdlib-only, in-memory, and serves over a
Unix socket — the LLM work moves to downstream consumers (router,
expander) so the hot loop polls at 200ms instead of 30s.
