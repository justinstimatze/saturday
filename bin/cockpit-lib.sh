#!/usr/bin/env bash
# cockpit-lib.sh — tmux-free helpers for bin/saturday-cockpit, split out so
# bin/test-cockpit-lib.sh can exercise them against a fixture $HOME without
# a multiplexer running. Sourced by bin/saturday-cockpit after the backend.

# abs_dir prints $1 as an absolute, symlink-resolved path, or fails if it
# isn't a directory. Every dir the cockpit launches into goes through this:
# a relative `cd 'src/project'` baked into a pane's start command breaks
# the moment that command is re-run from inside the pane's own cwd (a
# respawn), so start commands only ever carry absolute paths.
abs_dir() {
    (cd "$1" 2>/dev/null && pwd -P)
}

# encode_project_dir maps an absolute path to its directory name under
# ~/.claude/projects/. Claude Code replaces every character outside
# [A-Za-z0-9-] with '-', not only '/': ~/src/example.org is
# stored as -home-…-src-example-org, and a dotted parent such as
# ~/.cache becomes --cache. The encoding is lossy (a.b and a-b share a
# name), so resolve_last_session checks each transcript's recorded cwd
# rather than trusting the name alone.
encode_project_dir() {
    local LC_ALL=C
    printf '%s\n' "${1//[^A-Za-z0-9-]/-}"
}

# transcript_cwd prints the first "cwd" a transcript records — the dir its
# session was started in. grep -m1 stops at the first hit, so this stays
# cheap on 100k-line transcripts.
transcript_cwd() {
    local cwd
    cwd="$(grep -m1 -o '"cwd":"[^"]*"' "$1" 2>/dev/null)" || return 1
    cwd="${cwd#\"cwd\":\"}"
    printf '%s\n' "${cwd%\"}"
}

# resolve_last_session prints "<session-id><TAB><last-written>" for the
# newest transcript that was started in $1, or fails if there is none.
resolve_last_session() {
    local abs projdir f
    abs="$(abs_dir "$1")" || return 1
    projdir="$HOME/.claude/projects/$(encode_project_dir "$abs")"
    [ -d "$projdir" ] || return 1
    while IFS= read -r f; do
        [ "$(transcript_cwd "$f")" = "$abs" ] || continue
        printf '%s\t%s\n' "$(basename "$f" .jsonl)" "$(date -r "$f" '+%F %H:%M')"
        return 0
    done < <(find "$projdir" -maxdepth 1 -name '*.jsonl' -printf '%T@ %p\n' 2>/dev/null |
        sort -rn | cut -d' ' -f2-)
    return 1
}
