# Saturday — Stage (window choreography)

A Go sidecar that surfaces and highlights the Claude Code session Saturday is
addressing — on demand and proactively on inject — then de-emphasizes it when
the task completes. The flat-screen, here-now precursor to the ROADMAP's VR
"focus-driven window repositioning."

Stdlib-only Go. Shells out to `tmux`. No external deps.

## Two layers

Saturday lives in **tmux panes inside terminal windows**, so the problem splits
along that seam:

- **Layer A — tmux** (built): panes/windows *inside* a terminal. `select-window`
  / `select-pane` to foreground the addressed pane within its session, plus a
  reversible border tint + pane title. No Wayland involvement. Honest reach: it
  cannot raise a session's *terminal* across OS windows when each project is its
  own `cc-<proj>` tmux session — that's Layer B.
- **Layer B — OS windows** (stub): the terminal emulator window itself, browser
  windows. Hits Wayland's no-client-positioning wall. Decision: target
  **Hyprland/Sway** (`hyprctl` + `wlr-foreign-toplevel`), not GNOME. Lives behind
  the `WindowSource` interface as `--backend hyprland` until the compositor
  switch lands.

## Wire

One Unix socket; every connection is a bidirectional peer. Mayor dials in and
sends command lines; observers (e.g. `saturday-thinking`) dial in and read the
activity stream.

```jsonc
// commands in  (mayor → stage)
{"type":"focus",     "session_id":"<uuid>", "project":"lucida", "pane_id":"%5", "cwd":"/path"}
{"type":"restore",   "session_id":"<uuid>", "project":"lucida"}
{"type":"highlight", "session_id":"<uuid>", "project":"lucida", "pane_id":"%5", "level":"active|done|dim"}

// events out  (stage → all)
{"type":"window_activity", "source":"tmux", "tmux_session":"cc-lucida", "pane_id":"%5", "cwd":"/path", "focused":true, "ts":...}
```

Mayor emits `focus` from `commitInject`'s tmux branch (only after an inject has
cleared the confidence gate, so it's confident by construction) and `restore`
from `removePending` (covers completion, TTL expiry, and interruption). Highlight
state is keyed by `session_id`, so `restore` needs only the id and is a clean
no-op for sessions stage never touched (direct-write / headless injects).

## Privacy

The tmux activity stream only observes sessions whose name matches `--allow`
(default `^cc-` — the CC session naming from V0.1.5), so it is **CC-sessions-only
and safe by construction**. Everything else is never inspected. Browser windows
arrive only via the Hyprland backend with its own class allowlist (stubbed).
`session_id` is empty on tmux events (tmux doesn't know the CC UUID); join
`cwd` → CC session via the watcher.

## Run

```bash
cd saturday-stage
go run main.go                                   # /tmp/saturday-stage.sock, tmux backend, ^cc- allowlist
go run main.go --backend tmux --allow '^cc-'     # explicit
go run main.go --no-relocate                     # highlight only, no focus motion
```

Mayor connects with `--stage-sock /tmp/saturday-stage.sock` (empty = disabled).
`bin/saturday-stack` brings stage up headless and wires it into mayor.
