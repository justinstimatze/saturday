# Changelog

Saturday had milestone labels (V0.2.x, V0.3.1) in `ROADMAP.md` before it had
git tags. v0.4.0 is the first tag and covers everything since V0.3.1
(2026-07-14).

## v0.4.0 — 2026-09-29

### saturday-cockpit (new)

A tmux launcher that runs one `claude` per project in a tiled grid, for
driving several sessions from one screen.

- `saturday-cockpit <dir> ...` builds the grid, and `add` grows it. Every
  pane gets a slot, which Alt+1..9 (and Alt+letter past 9) jumps to and
  zooms. Alt+0 returns to the overview.
- `--resume` resumes each dir's last session without the picker. It finds
  dirs with dots and other punctuation in their names, and aborts before
  building anything if a dir has no session (`--allow-fresh` opts back in).
  It never picks a session another pane already holds.
- A pane manifest (`~/.local/state/saturday-cockpit/<session>.tsv`) records
  each pane's dir, flags, title and session. That makes these work:
  - panes addressable by name (`--resume docs`);
  - launch flags remembered across a cold restart;
  - a bare `saturday-cockpit --resume` restoring the whole last cockpit.
- `status` lists slot, name, state (busy, idle, suspended, dead), dir and
  session per pane.
- `restart [slot|name]` respawns a pane's claude in place on the same
  session, including the pane running the command. `wake` un-suspends a
  pane after ctrl+z, and so does the Alt+N jump to it.
- `watch -- <cmd>` adds a non-claude pane (a log tail, a live view)
  without moving focus.
- `bin/saturday-cockpit-term` opens the cockpit in a terminal window of
  its own, for callers that have none.
- Other additions:
  - `--title` names a pane;
  - `--remote-control` passes through to claude;
  - `--pellicle` adds a status-strip pane pair;
  - `--boot` plays an opt-in staggered "boot ritual" reveal on launch.
- `COCKPIT_BACKEND=ttmux` drives [statico/ttmux](https://github.com/statico/ttmux)
  instead of tmux. It's opt-in, and was last checked against ttmux v0.6.4.
  `restart`, `wake`, `watch` and `--boot` need tmux.

### saturday-stage (new)

- A window-choreography sidecar that mayor drives: it zooms or tiles the
  cockpit toward the pane being talked about, with grid-aware emphasis and
  a smoothed resize tween.

### Voice

- `saturday-voice`: realtime voice against a hosted `moshi-server`
  (Modal or Runpod). It streams TTS, and preemptive generation starts
  replying at a suspected pause instead of a confirmed one.
- Fixes for cold-start UX, the onset click and output headroom. A content
  gate, a quick-acknowledgement pool, and clarifying questions in the
  orchestrator.
- Spoken output is scored against the persona's voice rules (cope-gate),
  and the persona prompt carries a curated list of verbal tics to avoid.

### saturday-backend (new)

- Relays session state through Google Drive and writes back a session
  inventory. The stack skips it cleanly when `DRIVE_FOLDER_ID` is unset.

### Stack and tooling

- `saturday-stack` brings up stage, guards against a duplicate headless
  launch, and has `doctor` check the client-detached hook and the audio
  pidfile.
- `saturday-mayor` hook and state sockets are owner-only.
- The pre-commit hook runs shellcheck and the cockpit's fixture tests. It
  refuses a staged line or commit message that carries a local home path.
- The README install section is now one walkthrough.
