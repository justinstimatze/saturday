#!/usr/bin/env bash
# cockpit-backend-ttmux.sh — ttmux (https://github.com/statico/ttmux) backend
# for bin/saturday-cockpit. Beta software as of v0.6.4 (2026-09-20) — this
# backend is opt-in (COCKPIT_BACKEND=ttmux) and not the default for exactly
# that reason.
#
# ttmux has no per-pane arbitrary tag store, no per-keypress shell-out, no
# respawn-pane, no remain-on-exit, and no pane-lifecycle hooks. See
# ROADMAP.md rank 17 for what a future ttmux release would need to close
# them; short version of each gap and its fix here:
#
#   - Slot/title tracking moves from a tmux pane user-option to the sidecar
#     JSON file _ttmux_sidecar_path prints, pruned against a live
#     `list-panes --json` on every read (same auto-GC tmux got for free
#     from dropping a dead pane's options).
#   - Alt+N hotkeys can't resolve "which pane is slot N" at keypress time —
#     a ttmux key binding is a fixed string set at config-load, not a
#     command evaluated when the key is pressed. backend_bind_hotkeys
#     recomputes concrete `select-pane -t %id` bindings and pushes them via
#     `set-option` every time this script calls it (same call sites
#     bind_hotkeys already had for the tmux backend).
#   - Border labels are precomputed and pushed via `rename-pane`, not a
#     live format expression — backend_apply_border_format reads the
#     sidecar's raw title/basename (never ttmux's own already-labeled
#     title) so repeated calls don't compound into "1: 1: name".
#   - A pane that exits on its own (crash, /exit) does not get an even
#     re-tile the way tmux's after-kill-pane hook forces (ttmux has no
#     lifecycle-hook concept at all, and bin/saturday-cockpit never calls
#     kill-pane itself outside --boot, which refuses under this backend).
#     Verified live this is narrower than "nothing happens": ttmux's own
#     tiling engine does reclaim the dead pane's space for a neighbor, the
#     same way tmux behaves *without* that hook — what's missing is only
#     the snap-back-to-even step, not space reclamation itself. Accepted
#     limitation, not solved here.
#   - `status` shows less: ttmux's list-panes exposes
#     id/title/width/height/active/window, no pid and no cwd, so
#     backend_pane_details fills those from the sidecar or leaves them "-",
#     and STATE can't read a pane's Claude Code session record.
#
# --boot is not implemented here at all — the Steel Battalion ritual is
# built on respawn-pane, remain-on-exit + #{pane_dead} polling, and a
# custom-pane-option sync latch, none of which exist in ttmux. The guard
# in bin/saturday-cockpit's --boot branch refuses before any backend call
# happens, so nothing below needs to handle it.
#
# First verified live against ttmux 0.3.1; re-run against 0.6.4 (launch,
# add --title, status, the watch/restart refusals, stop) with no changes
# needed. Checked originally: list-sessions/list-panes JSON shape, split-window's return
# value and COMMAND-string form, rename-pane's effect on list-panes' title
# field, select-layout, set-option on a keys.* binding and reading it back,
# resize-pane -Z, and the has-session-via-exit-code substitute (list-panes
# exits 3 with no session, 0 with one). One non-reproducible session crash
# was seen once during testing after a pane's command exited near-instantly
# post-split; a clean retry of the same sequence did not reproduce it —
# flagged as a live-checklist watch item, not a confirmed defect, given
# this build is a same-day release.

backend_require() {
    if ! command -v ttmux >/dev/null 2>&1; then
        echo "saturday-cockpit: ttmux not installed (cargo install --locked --git https://github.com/statico/ttmux --tag v0.6.4)." >&2
        exit 1
    fi
    export TTMUX_SESSION="$SESSION"
}

backend_check_not_nested() {
    if [ -n "${TTMUX_SESSION:-}" ] && [ "${TTMUX_SESSION}" != "$SESSION" ]; then
        echo "saturday-cockpit: already inside ttmux. Open a fresh terminal." >&2
        exit 1
    fi
}

# has-session has no ttmux verb of its own — list-panes exits 3 when no
# session is listening (API.md's own documented exit-code contract),
# 0 when one is, so a lightweight read-only call substitutes cleanly.
backend_has_session() { ttmux list-panes >/dev/null 2>&1; }
backend_kill_session() { ttmux kill-session -t "$SESSION"; }
backend_attach() { exec ttmux attach -t "$SESSION"; }

# ttmux's `new`/`new-session` takes no COMMAND argument (unlike tmux's) —
# confirmed live: a fresh session's first pane always starts an interactive
# shell. send-keys into it once it exists, same pattern API.md's own
# recipes use for "type the real command in after creating the workspace".
backend_new_session() {
    local dir="$1" cmd="$2"
    ttmux new -d -s "$SESSION"
    local first
    first="$(backend_list_pane_ids | tail -1)"
    ttmux send-keys -t "$first" "cd '$dir' && exec $cmd" Enter
    # add_pane/add_pane_pellicle call backend_set_dir themselves — this pane
    # is created directly, bypassing both, so nothing else will.
    backend_set_dir "$first" "$dir"
    echo "$first"
}

# $1 is accepted-but-ignored — ttmux has no cross-session listing; the
# session is scoped for this whole script's run via $TTMUX_SESSION (set in
# backend_require), not a per-call -t. Signature stays symmetric with the
# tmux backend so bin/saturday-cockpit's own call sites don't need an
# if/else per backend.
backend_list_pane_ids() {
    ttmux list-panes --json | jq -r '.panes[].id'
}

# backend_pane_details: same columns as the tmux backend's, from the slot
# sidecar. ttmux exposes no pid, dead flag or start command, so those come
# back as "-"/0, and cwd is the dir the pane was launched in.
backend_pane_details() {
    _ttmux_sidecar_load_pruned | jq -r 'to_entries[] |
        [.key, (.value.slot // "-"), "-", "0", "-", (.value.dir // "-"), (.value.title // "-"),
         (.value.kind // "-"), (.value.dir // "-"), (.value.cmd // "-"), (.value.sid // "-")]
        | map(if . == "" then "-" else . end) | @tsv'
}

backend_pane_count() { ttmux list-panes --json | jq '.panes | length'; }

# split-window DOES take a COMMAND directly (unlike new/new-session) —
# confirmed live: `ttmux split-window -t %1 'cd DIR && exec CMD'` runs it
# through the pane's shell with -c and prints the new pane's id, same
# shape as tmux's -P -F '#{pane_id}'.
#
# $target is a real pane id from add_pane_pellicle's second split, but
# add_pane passes $SESSION itself — valid for tmux (-t SESSION splits
# whatever pane is active there) but not for ttmux, whose -t takes only a
# pane id (confirmed live: "not a pane id: <session name>"). Resolve a
# session-shaped target to the most recently created pane first, the same
# "list-panes | tail -1" convention this script already uses everywhere
# else to find "the pane that just got made".
backend_split() {
    local target="$1" dir="$2" cmd="$3"
    case "$target" in
        %*) : ;;
        *) target="$(backend_list_pane_ids | tail -1)" ;;
    esac
    ttmux split-window -t "$target" "cd '$dir' && exec $cmd"
}

# No -b (before/anchor-above) and no -l (fixed line count) on ttmux's
# split-window — API.md documents only -t/-h/-v. Splits after $target
# instead of before it, then resizes down to $lines afterward. Positional
# placement (before vs. after) is cosmetic for the pellicle render strip;
# its own Stop-hook resize already self-heals the exact height on the
# next turn regardless (see add_pane_pellicle's existing comment about
# select-layout clobbering this the same way) — same self-heal covers an
# imprecise ttmux resize too, not a new gap this backend introduces.
backend_split_strip() {
    local target="$1" dir="$2" cmd="$3" lines="$4"
    local pane height shrink
    pane="$(ttmux split-window -t "$target" "cd '$dir' && exec $cmd")"
    height="$(ttmux list-panes --json | jq -r --arg id "$pane" '.panes[] | select(.id==$id) | .height')"
    if [ -n "$height" ] && [ "$height" -gt "$lines" ]; then
        shrink=$((height - lines))
        # -D shrinks the target pane (verified live: -U grew it instead —
        # resize-pane's direction flag is which edge of $pane to push, and
        # -U pushed its top edge outward/up, growing it).
        ttmux resize-pane -t "$pane" -D "$shrink" >/dev/null 2>&1 || true
    fi
    echo "$pane"
}

backend_capture() { ttmux capture-pane -t "$1" 2>/dev/null; }

backend_select_layout() { ttmux select-layout "$LAYOUT" >/dev/null 2>&1 || true; }
backend_select_pane() { ttmux select-pane -t "$1"; }

# ---- slot/title sidecar --------------------------------------------------
#
# One JSON file per session, colocated with ttmux's own per-user socket
# directory (same env resolution ttmux's proto::socket_dir() uses), so it
# lives next to the exact server it describes and cleans up the same way a
# stale socket would. Shape: {"<pane_id>": {"slot": N, "title": "..."}}.
# Pruned against a live list-panes on every read — the same auto-GC
# property tmux got for free from dropping a dead pane's user option.

_ttmux_state_dir() {
    local base="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}"
    echo "${base%/}/ttmux-$(id -u)"
}

_ttmux_sidecar_path() {
    echo "$(_ttmux_state_dir)/saturday-cockpit-slots-${SESSION}.json"
}

# Loads the sidecar (default {} if missing/empty/corrupt) and drops any
# pane id no longer in a live list-panes — same prune-on-read every caller
# below goes through, so nothing has to remember to GC separately.
_ttmux_sidecar_load_pruned() {
    local path live
    path="$(_ttmux_sidecar_path)"
    live="$(backend_list_pane_ids | jq -R . | jq -s .)"
    if [ -s "$path" ]; then
        jq --argjson live "$live" 'with_entries(select(.key as $k | $live | index($k)))' "$path" 2>/dev/null || echo '{}'
    else
        echo '{}'
    fi
}

# Read-modify-write with the pruned view as the base, so a dead pane's
# stale entry never resurfaces even if it's never explicitly deleted.
_ttmux_sidecar_write() {
    local new="$1" dir path tmp
    dir="$(_ttmux_state_dir)"
    mkdir -p "$dir" && chmod 0700 "$dir"
    path="$(_ttmux_sidecar_path)"
    tmp="${path}.tmp.$$"
    echo "$new" >"$tmp" && mv "$tmp" "$path"
}

_ttmux_sidecar_set_field() {
    local pane="$1" field="$2" value="$3" pruned
    pruned="$(_ttmux_sidecar_load_pruned)"
    _ttmux_sidecar_write "$(echo "$pruned" | jq --arg p "$pane" --arg f "$field" --arg v "$value" \
        '.[$p] = ((.[$p] // {}) + {($f): $v})')"
}

backend_used_slots() {
    _ttmux_sidecar_load_pruned | jq -r '[.[].slot // empty] | map(tonumber) | sort[]'
}
backend_get_slot() {
    _ttmux_sidecar_load_pruned | jq -r --arg p "$1" '.[$p].slot // empty'
}
backend_set_slot() { _ttmux_sidecar_set_field "$1" slot "$2"; }
backend_set_title() { _ttmux_sidecar_set_field "$1" title "$2"; }
# tmux backend has no-op equivalent (its live #{b:pane_current_path} ternary
# needs no stored dir) — ttmux exposes no cwd via list-panes at all, so this
# is the only way backend_apply_border_format can fall back to a directory
# basename instead of ttmux's own process-title default ("bash", a shell
# prompt string, ...).
backend_set_dir() { _ttmux_sidecar_set_field "$1" dir "$2"; }
backend_set_meta() { _ttmux_sidecar_set_field "$1" "$2" "$3"; }
backend_get_meta() { _ttmux_sidecar_load_pruned | jq -r --arg p "$1" --arg k "$2" '.[$p][$k] // empty'; }

# ---- hotkeys / border labels ---------------------------------------------
#
# Both rebuilt from the sidecar's CURRENT snapshot every time this is
# called — after initial launch and after every `add`, same call sites the
# tmux backend already needed (its version is idempotent either way, so
# the extra safety here costs it nothing).
backend_bind_hotkeys() {
    # slot is stored via jq --arg (always a string) — compare as a string,
    # not --argjson, or "3" == 3 silently never matches.
    local pruned pane_for_slot n l pane=10
    pruned="$(_ttmux_sidecar_load_pruned)"
    for n in 1 2 3 4 5 6 7 8 9; do
        pane_for_slot="$(echo "$pruned" | jq -r --arg n "$n" 'to_entries[] | select(.value.slot==$n) | .key' | head -1)"
        if [ -n "$pane_for_slot" ]; then
            ttmux set-option "keys.alt+$n" "select-pane -t $pane_for_slot ; resize-pane -Z" >/dev/null 2>&1 || true
        fi
    done
    for l in "${COCKPIT_HOTKEY_LETTERS[@]}"; do
        pane_for_slot="$(echo "$pruned" | jq -r --arg n "$pane" 'to_entries[] | select(.value.slot==$n) | .key' | head -1)"
        if [ -n "$pane_for_slot" ]; then
            ttmux set-option "keys.alt+$l" "select-pane -t $pane_for_slot ; resize-pane -Z" >/dev/null 2>&1 || true
        fi
        pane=$((pane + 1))
    done
    ttmux set-option "keys.alt+0" "run toggle-zoom" >/dev/null 2>&1 || true
}

# Precompute-and-push, unlike tmux's live ternary: reads the sidecar's raw
# title/dir (never ttmux's own list-panes title, which may already carry a
# previous call's "N: " prefix, and which ttmux exposes no cwd for at all)
# so repeated calls don't compound and the fallback is a directory
# basename, not ttmux's default process-title string ("bash", a shell
# prompt, ...).
backend_apply_border_format() {
    local pruned id slot title dir label
    pruned="$(_ttmux_sidecar_load_pruned)"
    while IFS=$'\t' read -r id slot title dir; do
        [ -z "$id" ] && continue
        if [ "$slot" != "null" ] && [ -n "$slot" ]; then
            if [ "$slot" -le 9 ]; then
                label="$slot"
            else
                label="${COCKPIT_HOTKEY_LETTERS[$((slot - 10))]:-$slot}"
            fi
        else
            label=""
        fi
        if [ -z "$title" ] || [ "$title" = "null" ]; then
            if [ -n "$dir" ] && [ "$dir" != "null" ]; then
                title="$(basename "$dir")"
            else
                title="$id"
            fi
        fi
        ttmux rename-pane -t "$id" "${label:+$label: }$title" >/dev/null 2>&1 || true
    done < <(echo "$pruned" | jq -r 'to_entries[] | [.key, (.value.slot // "null"), (.value.title // "null"), (.value.dir // "null")] | @tsv')
}
