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

same() { # same <name> <got> <want>
    if [ "$2" = "$3" ]; then
        echo "ok   $1"
    else
        echo "FAIL $1: got '$2', want '$3'"
        fails=$((fails + 1))
    fi
}

same "strip_resume drops --resume <id>" \
    "$(strip_resume 'claude --model m --resume 3f2a-9 --remote-control')" "claude --model m --remote-control"
same "strip_resume drops a bare --resume" \
    "$(strip_resume 'claude --resume --dangerously-skip-permissions')" "claude --dangerously-skip-permissions"
same "strip_resume drops --resume-last" "$(strip_resume 'claude --resume-last')" "claude"

same "resume_id finds the id" "$(resume_id 'claude --resume 3f2a-9 --rc')" "3f2a-9"
same "resume_id, bare --resume" "$(resume_id 'claude --resume --rc')" ""

start="\"cd '/home/u/src/example.org' && exec claude --model m --resume 396903b6\""
same "start_cmd_dir"                "$(start_cmd_dir "$start")" "/home/u/src/example.org"
same "start_cmd_cmd"                "$(start_cmd_cmd "$start")" "claude --model m --resume 396903b6"
same "start_cmd_dir, unknown shape" "$(start_cmd_dir "bash")" ""

mkdir -p "$HOME/.claude/sessions"
printf '{"pid":4242,"sessionId":"ceddea17","cwd":"/w/notes","name":"Notes","status":"busy"}' \
    > "$HOME/.claude/sessions/4242.json"
same "session_field sessionId" "$(session_field 4242 sessionId)" "ceddea17"
same "session_field name"      "$(session_field 4242 name)"      "Notes"
same "session_field, no record" "$(session_field 4243 name || echo miss)" "miss"

SESSION=cc-test XDG_STATE_HOME="$fixture/state"
printf '3\tclaude\t/w/b\t-\tB\tclaude\ts3\n1\tclaude\t/w/a\tAlpha\t-\tclaude --rc\ts1\n' | manifest_write
same "manifest sorted by slot" "$(manifest_rows | cut -f1 | tr '\n' ' ')" "1 3 "
same "manifest row for slot"   "$(manifest_row_for_slot 3 | cut -f3)" "/w/b"
same "name matches a session name"  "$(manifest_slots_matching b)" "3"
same "name matches a title"         "$(manifest_slots_matching ALPHA)" "1"
same "name matches a dir basename"  "$(manifest_slots_matching a)" "1"
same "unknown name matches nothing" "$(manifest_slots_matching zed)" ""
same "manifest_names"               "$(manifest_names)" "Alpha, B"
same "manifest_row_for_dir"         "$(manifest_row_for_dir /w/a | cut -f1)" "1"
same "manifest_row_for_dir, none"   "$(manifest_row_for_dir /w/zz)" ""
same "transcript_exists"            "$(transcript_exists "$work/a.b" sess-ab-dot && echo y)" "y"
same "transcript_exists, gone"      "$(transcript_exists "$work/a.b" nope || echo n)" "n"
same "dash/undash round trip"  "$(undash "$(dash '')")|$(undash "$(dash 'a	b')")" "|a b"

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
echo "all passed"
