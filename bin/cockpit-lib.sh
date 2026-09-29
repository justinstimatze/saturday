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

# strip_resume drops `--resume [<id>]` and `--resume-last` from a command
# string, leaving the launch flags a pane should keep across a resume or
# restart (--model, --remote-control, --dangerously-skip-permissions).
strip_resume() {
    local words=() out=() skip=0 w
    read -ra words <<< "$1"
    for w in "${words[@]}"; do
        if [ "$skip" -eq 1 ]; then
            skip=0
            [[ "$w" == -* ]] || continue
        fi
        case "$w" in
            --resume) skip=1; continue ;;
            --resume-last) continue ;;
        esac
        out+=("$w")
    done
    printf '%s\n' "${out[*]}"
}

# resume_id prints the session id after `--resume` in a command string, if
# there is one.
resume_id() {
    local words=() w prev=""
    read -ra words <<< "$1"
    for w in "${words[@]}"; do
        if [ "$prev" = "--resume" ] && [[ "$w" != -* ]]; then
            printf '%s\n' "$w"
            return 0
        fi
        prev="$w"
    done
}

# start_cmd_dir / start_cmd_cmd split a pane start command of the shape this
# script launches, `cd '<dir>' && exec <cmd>`, into its two halves. tmux's
# #{pane_start_command} wraps the whole thing in double quotes; both forms
# are accepted. Anything else prints nothing.
start_cmd_dir() {
    local s="${1#\"}"
    s="${s%\"}"
    [[ "$s" == "cd '"*"' && exec "* ]] || return 0
    s="${s#cd \'}"
    printf '%s\n' "${s%%\' && exec *}"
}
start_cmd_cmd() {
    local s="${1#\"}"
    s="${s%\"}"
    [[ "$s" == *" && exec "* ]] || return 0
    printf '%s\n' "${s#* && exec }"
}

# session_field prints one string field from Claude Code's per-process
# record, ~/.claude/sessions/<pid>.json (sessionId, name, cwd, status).
# Every cockpit pane execs claude, so a pane's pid is that process. The
# record is undocumented and is deleted when the process exits, so every
# caller treats a miss as "unknown", never as an error.
session_field() {
    local f="$HOME/.claude/sessions/$1.json" v
    [ -f "$f" ] || return 1
    v="$(grep -o "\"$2\":\"[^\"]*\"" "$f" 2>/dev/null)" || return 1
    v="${v%%$'\n'*}"
    v="${v#*\":\"}"
    printf '%s\n' "${v%\"}"
}

# ---- pane manifest ----------------------------------------------------------
#
# One row per slotted pane, so a cockpit can be restored, restarted or
# addressed by name after its panes (and their session records) are gone:
#   slot kind dir title name cmd last_sid
# kind is claude, pellicle or watch; title is an explicit --title; name is
# the session's own name from Claude Code (/rename); cmd is the launch
# command minus any --resume. Tab-separated, "-" for an empty field (bash's
# read collapses consecutive tabs, so an empty field would shift the rest).
# Rewritten from the live panes on launch, add, status and stop, which also
# means a pane closed on its own drops out at the next refresh. After a
# crash or reboot the file holds whatever the last refresh saw.

manifest_path() {
    printf '%s/saturday-cockpit/%s.tsv\n' "${XDG_STATE_HOME:-$HOME/.local/state}" "$SESSION"
}

# manifest_rows prints the data rows (no header), or nothing.
manifest_rows() {
    local p
    p="$(manifest_path)"
    [ -f "$p" ] || return 0
    grep -v '^#' "$p" || true
}

manifest_row_for_slot() {
    manifest_rows | awk -F'\t' -v s="$1" '$1 == s { print; exit }'
}

# manifest_write replaces the manifest with the rows on stdin, sorted by
# slot, via a temp file so a reader never sees half a file.
manifest_write() {
    local p tmp
    p="$(manifest_path)"
    mkdir -p "$(dirname "$p")"
    tmp="$p.tmp.$$"
    {
        printf '# slot\tkind\tdir\ttitle\tname\tcmd\tlast_sid  (saturday-cockpit pane manifest)\n'
        sort -t$'\t' -k1,1n
    } >"$tmp" && mv "$tmp" "$p"
}

# manifest_slots_matching prints the slot of every row whose title, Claude
# session name, or dir basename equals $1, ignoring case — the names status
# shows, so "docs" finds the pane whose session was renamed Docs even
# though its dir is example.org.
manifest_slots_matching() {
    manifest_rows | awk -F'\t' -v w="$1" '
        function base(p) { sub(/.*\//, "", p); return p }
        BEGIN { w = tolower(w) }
        tolower($4) == w || tolower($5) == w || tolower(base($3)) == w { print $1 }'
}

# manifest_names lists every name a row answers to, for error messages.
manifest_names() {
    manifest_rows | awk -F'\t' '
        { n = ($4 != "-") ? $4 : ($5 != "-") ? $5 : $3; sub(/.*\//, "", n); printf "%s%s", sep, n; sep = ", " }
        END { if (sep) print "" }'
}

# dash / undash convert between an empty value and the manifest's "-".
# dash also flattens tabs, the one character a field can't hold.
dash() {
    local v="${1//$'\t'/ }"
    printf '%s\n' "${v:--}"
}
undash() { [ "$1" = "-" ] && return 0; printf '%s\n' "$1"; }
