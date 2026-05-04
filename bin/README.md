# Saturday — bin/

Helper executables for the Saturday voice loop.

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

**Env overrides** (rarely needed):

| var            | default                                       | meaning                                          |
|----------------|-----------------------------------------------|--------------------------------------------------|
| `SATURDAY_DIR` | `$HOME/Documents/saturday`                    | repo root containing `saturday-audio/`           |
| `SOCK`         | `/tmp/saturday-audio.sock`                    | mayor↔audio Unix socket                          |
| `AUDIO_VENV`   | `$SATURDAY_DIR/saturday-audio/.venv`          | venv with faster-whisper, kokoro-onnx, etc.      |

The script sanity-checks before constructing the session: aborts with a
helpful message if `saturday-watcher` / `saturday-mayor` aren't on
`PATH` or in `$(go env GOPATH)/bin`, if `saturday-audio/.venv/bin/activate`
is missing, or if you're already inside tmux.

## Install

The Go binaries are installed via `make install` from the workspace
root. The bash launchers in this directory ship separately — copy them
to the same `bin` directory once:

```bash
cp bin/saturday-claude bin/saturday-stack "$(go env GOPATH)/bin"
chmod +x "$(go env GOPATH)/bin/saturday-claude" \
         "$(go env GOPATH)/bin/saturday-stack"
```
