# Saturday — Backend

Phase 1 of `SATURDAY-VOICE-NATIVE.md` §2: polls a private Google Drive
folder for plain-language notes (written there by Claude's voice mode via
its own first-party Drive connector — see the doc for the tested mechanism)
and turns each one into a real inject, using the same router/expander
pipeline `saturday-mayor` runs for voice utterances and the `inject`/
`settle`/`watcherclient` packages both binaries share.

No custom voice client, no MCP connector — the note is the whole interface.

## Build

From the workspace root:

```bash
make install        # → $(go env GOPATH)/bin/saturday-backend (and the others)
```

## Setup: Google Cloud (one-time, only you can do this)

`saturday-backend` needs its own OAuth grant to read the Drive folder —
Claude's own connection to Drive is a separate credential and can't be
reused by a standalone program. Steps, done once:

1. **Create or select a Google Cloud project** at
   [console.cloud.google.com](https://console.cloud.google.com) — the free
   tier is enough, no billing account required for this.
2. **Enable the Drive API** for that project: APIs & Services → Library →
   search "Google Drive API" → Enable.
3. **Create an OAuth client ID**: APIs & Services → Credentials → Create
   Credentials → OAuth client ID → Application type **Desktop app**. Give
   it any name.
4. **Download the client secret JSON** and save it to
   `~/.config/saturday/drive-credentials.json` (or pass `--drive-credentials
   <path>`). A browser download typically lands world-readable — tighten it:
   `chmod 600 ~/.config/saturday/drive-credentials.json`. (Nothing in
   `saturday-backend` touches this file's permissions itself — only the
   *derived* OAuth token gets written at `0600` automatically, by
   `--drive-login`.)
5. **Find the folder ID** of the Drive folder Claude's connector writes
   notes to — it's the segment after `/folders/` in the folder's URL.

## Setup: one-time login

```bash
saturday-backend --drive-credentials ~/.config/saturday/drive-credentials.json \
  --drive-login
```

This opens a URL to approve in a browser once, then caches a token (with a
refresh token, so this step never repeats) to
`~/.config/saturday/drive-token.json` at `0600`. The requested scope is
`drive.readonly` — this program can never write to or delete anything in
your Drive.

## Setup: ANTHROPIC_API_KEY and the LLM cache

Same convention as `saturday-mayor` — see its README for the full XDG
search order. Short version:

```bash
mkdir -p ~/.config/saturday
echo 'ANTHROPIC_API_KEY=sk-...' > ~/.config/saturday/config
chmod 600 ~/.config/saturday/config
```

## Run

`saturday-watcher` must be running first — same as `saturday-mayor`, the
backend queries it for live session state on every note:

```bash
saturday-backend --drive-folder-id <folder-id>
```

Flags mirror `saturday-mayor` where the same concept applies
(`--sock`, `--env`, `--cache`, `--conf-threshold`, `--collision-wait`,
`--collision-max`, `--inject-direct-threshold`, `--dry-run`), plus:

| Flag | Default | Meaning |
|---|---|---|
| `--drive-credentials` | `~/.config/saturday/drive-credentials.json` | OAuth client secret from Cloud Console |
| `--drive-token` | `~/.config/saturday/drive-token.json` | cached token, written by `--drive-login` |
| `--drive-folder-id` | *(required)* | the folder to poll |
| `--poll-interval` | `15s` | how often to check for new notes |
| `--cursor` | `~/.config/saturday/backend-cursor.json` | what's already been processed |

## What this doesn't do yet

No completion reports, no push notifications, no das-blinkenlights corner
tag — all deferred to Phase 2 (`SATURDAY-VOICE-NATIVE.md` §4), which needs
a completion signal to clear against. This phase's job ends at "the note
lands as an inject in the right pane."
