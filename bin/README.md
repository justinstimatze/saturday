# Saturday — bin/

Helper executables for the Saturday voice loop.

## Quick start

Two commands, in this order:

```bash
export DRIVE_FOLDER_ID=<your Drive folder id>   # once per shell — see saturday-backend/README.md
saturday-stack                                   # watcher, mayor, audio, stage, backend — headless legs + panes
saturday-cockpit ~/Documents/spar ~/Documents/whatever   # your project panes, whichever you want open today
```

`saturday-stack` first: it brings up everything that has to already be
running before a session is useful (watcher, `saturday-stage`, the
Drive-relay backend). `saturday-cockpit` second, for whichever project
panes you actually want today — deliberately a separate step rather than
a fixed list, since that list changes daily. Re-run either command any
time; both attach to what's already running instead of duplicating it.

## `saturday-claude`

Bash wrapper that starts (or attaches to) a tmux session running `claude`
in the current directory's project. All arguments forward to `claude`.

```bash
saturday-claude                       # fresh interactive claude in tmux
saturday-claude --resume <sid>        # resume a specific session, in tmux
saturday-claude --model opus          # any flag claude accepts
```

Project = `$PWD`. Tmux session name = `cc-<basename PWD>`. Re-running
from the same directory attaches the existing session if alive.

**Why:** Saturday's voice loop sends keystrokes via `tmux send-keys` into
the live pane running `claude` (V0.1.5 primary inject path in
`saturday-mayor`). Mayor finds the right pane by descending each tmux
pane's `/proc` tree to locate a `claude` process and matching its `cwd`
against the target session. For that to succeed, your `claude` has to be
inside a tmux pane.

`saturday-claude` is the friction-free way to make that happen — old
sessions and direct `claude` invocations keep working (mayor falls back
to JSONL-direct-write or headless `--resume --print` paths for them),
but Saturday can only inject *instantly* into sessions started this way
(or otherwise inside tmux).

If you're already inside tmux, the script declines and tells you to just
run `claude` directly — no nested tmux sessions.

## Optional shell function

If you want `claude` itself to always wrap into tmux, add this to
`~/.bashrc` or `~/.zshrc`:

```bash
claude() {
    if [ -n "$TMUX" ]; then
        command claude "$@"
    else
        local s="cc-$(basename "$PWD")"
        tmux new-session -A -s "$s" -c "$PWD" "command claude $*"
    fi
}
```

`command claude` bypasses the function to call the real binary inside
tmux. Opt-in only — Saturday doesn't install this for you.

## `saturday-stack`

Bash wrapper that starts (or attaches to) a 3-pane tmux session named
`saturday-stack` running the full Saturday voice loop:

- **Pane 0** — `saturday-watcher` (polls `~/.claude/projects/`, exposes
  per-session state on its Unix socket)
- **Pane 1** — `saturday-mayor --audio-sock /tmp/saturday-audio.sock`
- **Pane 2** — audio sidecar (`saturday-audio/main.py` inside its venv)
  with focus, since SPACEBAR-mute lives there

```bash
saturday-stack          # bring up the whole stack, attach
saturday-stack          # re-running attaches the existing session
```

Idempotent: re-runs attach. `remain-on-exit on` keeps each pane around
after its process exits so you can read crash output. `tmux
respawn-pane` to restart a single pane; `tmux kill-session -t
saturday-stack` to start fresh.

Also runs headless (no pane): `saturday-watcher`, `saturday-stage`
(window choreography), and `saturday-backend` (phone-voice Drive relay —
see `saturday-backend/README.md`).

**Env overrides:**

| var                     | default                              | meaning                                          |
|-------------------------|---------------------------------------|--------------------------------------------------|
| `SATURDAY_DIR`          | `$HOME/Documents/saturday`            | repo root containing `saturday-audio/`           |
| `SOCK`                  | `/tmp/saturday-audio.sock`            | mayor↔audio Unix socket                          |
| `AUDIO_VENV`            | `$SATURDAY_DIR/saturday-audio/.venv`  | venv with faster-whisper, kokoro-onnx, etc.      |
| `DRIVE_FOLDER_ID`       | *(required)*                          | Drive folder Claude's voice-mode connector writes notes to |
| `BACKEND_POLL_INTERVAL` | `5s`                                   | how often `saturday-backend` polls Drive         |

The script sanity-checks before constructing the session: aborts with a
helpful message if `saturday-watcher` / `saturday-mayor` aren't on
`PATH` or in `$(go env GOPATH)/bin`, if `saturday-audio/.venv/bin/activate`
is missing, or if you're already inside tmux.

## `saturday-cockpit`

Bash wrapper that opens one tmux window holding every project you name as
a pane, so `saturday-stage` can zoom or salience-tile the addressed pane
on inject — the alternative to one `saturday-claude` session per project
spread across separate terminals.

```bash
saturday-cockpit ~/Documents/lucida ~/Documents/saturday ~/src/groupchat
saturday-cockpit add <dir> [<dir> ...]   # add pane(s) to a running cockpit
saturday-cockpit stop                    # kill the cockpit window/session
saturday-cockpit status                  # list panes (pane_id, pid, cwd)
```

Session name is `cc-cockpit` (matches stage's activity allowlist). Hotkeys
work anywhere in tmux once a cockpit has run once: `Alt+1`..`9` jumps to
pane N zoomed full-screen (press again to un-zoom), `Alt+0` forces the
tiled overview. See the script's own header comment for `add --resume`,
`add --pellicle`, and env overrides (`COCKPIT`, `COCKPIT_TILE`).

## Install

The Go binaries are installed via `make install` from the workspace
root. The bash launchers in this directory ship separately — copy them
to the same `bin` directory once:

```bash
cp bin/saturday-claude bin/saturday-stack bin/saturday-cockpit "$(go env GOPATH)/bin"
chmod +x "$(go env GOPATH)/bin/saturday-claude" \
         "$(go env GOPATH)/bin/saturday-stack" \
         "$(go env GOPATH)/bin/saturday-cockpit"
```
