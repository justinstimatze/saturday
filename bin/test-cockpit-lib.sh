#!/usr/bin/env bash
# test-cockpit-lib.sh — fixture tests for bin/cockpit-lib.sh. Builds a fake
# $HOME with ~/.claude/projects/ entries shaped like Claude Code's own, so
# no real transcripts are read. Run via `make test`, or directly.
# COCKPIT_LIB overrides which lib is sourced (for checking a candidate fix).

set -uo pipefail

lib="${COCKPIT_LIB:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/cockpit-lib.sh}"
# shellcheck source=cockpit-lib.sh
source "$lib"

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
HOME="$fixture/home"
work="$(cd "$fixture" && pwd -P)/work"
mkdir -p "$HOME/.claude/projects" "$work/a.b" "$work/a-b" "$work/x_y" "$work/plain" "$work/never"

# Claude Code's own project-dir encoding, written independently of the lib's
# encode_project_dir so a wrong lib can't build fixtures that agree with it.
claude_encode() { printf '%s' "$1" | LC_ALL=C sed 's/[^A-Za-z0-9-]/-/g'; }

# transcript <dir> <session-id> <recorded-cwd> <age-seconds>
transcript() {
    local proj
    proj="$HOME/.claude/projects/$(claude_encode "$1")"
    mkdir -p "$proj"
    printf '{"type":"user","cwd":"%s","sessionId":"%s"}\n' "$3" "$2" > "$proj/$2.jsonl"
    touch -d "@$(( $(date +%s) - $4 ))" "$proj/$2.jsonl"
}

# a.b and a-b share one encoded project dir; a-b's session is newer.
transcript "$work/a.b" sess-ab-dot "$work/a.b" 300
transcript "$work/a-b" sess-ab-dash "$work/a-b" 100
transcript "$work/x_y" sess-underscore "$work/x_y" 100
transcript "$work/plain" sess-plain-old "$work/plain" 500
transcript "$work/plain" sess-plain-new "$work/plain" 50

fails=0
expect() { # expect <name> <dir> <want-session-id or empty for a miss>
    local got
    got="$(resolve_last_session "$2" | cut -f1)"
    if [ "$got" = "$3" ]; then
        echo "ok   $1"
    else
        echo "FAIL $1: got '${got}', want '${3}'"
        fails=$((fails + 1))
    fi
}

expect "dotted dir resolves"                  "$work/a.b"   sess-ab-dot
expect "collision picks the matching cwd"     "$work/a-b"   sess-ab-dash
expect "underscore dir resolves"              "$work/x_y"   sess-underscore
expect "newest transcript wins"               "$work/plain" sess-plain-new
expect "relative path resolves"               "$(realpath --relative-to="$PWD" "$work/plain")" sess-plain-new
expect "dir with no project is a miss"        "$work/never" ""
expect "missing dir is a miss"                "$work/gone"  ""

# a project dir whose transcripts were all started somewhere else is a miss
rm "$HOME/.claude/projects/$(claude_encode "$work/x_y")/sess-underscore.jsonl"
transcript "$work/x_y" sess-elsewhere "$work/x.y" 10
expect "cwd mismatch is a miss"               "$work/x_y"   ""

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
echo "all passed"
