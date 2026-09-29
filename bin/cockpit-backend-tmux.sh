#!/usr/bin/env bash
# cockpit-backend-tmux.sh — tmux backend for bin/saturday-cockpit.
#
# Behavior-preserving: every function here is the same tmux invocation
# bin/saturday-cockpit made directly before the backend split existed, just
# named and moved. See bin/cockpit-backend-ttmux.sh for the other backend
# and ~/.claude/plans/wobbly-honking-valley.md for why the two diverge
# where they do.
#
# Not used by --boot: the Steel Battalion ritual's edge-splits, growth
# animation, and respawn-based reveal stay inline in bin/saturday-cockpit
# as raw tmux calls. --boot is tmux-only (see the guard at its call site in
# bin/saturday-cockpit), so routing that code through this indirection
# would buy nothing and would risk the one feature that's actually been
# tuned and shipped (v3, 60e63a5).

backend_require() {
    if ! command -v tmux >/dev/null 2>&1; then
        echo "saturday-cockpit: tmux not installed (apt install tmux)." >&2
        exit 1
    fi
}

backend_check_not_nested() {
    if [ -n "${TMUX:-}" ]; then
        echo "saturday-cockpit: already inside tmux. Open a fresh terminal." >&2
        exit 1
    fi
}

backend_has_session() { tmux has-session -t "$SESSION" 2>/dev/null; }
backend_kill_session() { tmux kill-session -t "$SESSION"; }
backend_attach() { exec tmux attach-session -t "$SESSION"; }

backend_new_session() {
    local dir="$1" cmd="$2"
    tmux new-session -d -s "$SESSION" -n "cockpit" "cd '$dir' && exec $cmd"
}

backend_list_pane_ids() {
    local target="${1:-$SESSION}"
    tmux list-panes -s -t "$target" -F '#{pane_id}'
}

# backend_pane_details: one tab-separated row per pane in the session, every
# window (-s), empty values as "-" (see the manifest note in cockpit-lib.sh):
#   pane_id slot pid dead start_command cwd title
backend_pane_details() {
    tmux list-panes -s -t "$SESSION" -F \
        "#{pane_id}	#{?@cockpit_slot,#{@cockpit_slot},-}	#{pane_pid}	#{pane_dead}	#{?pane_start_command,#{pane_start_command},-}	#{pane_current_path}	#{?@cockpit_title,#{@cockpit_title},-}"
}

backend_pane_count() { tmux list-panes -s -t "$SESSION" | wc -l; }

backend_split() {
    local target="$1" dir="$2" cmd="$3"
    tmux split-window -t "$target" -P -F '#{pane_id}' "cd '$dir' && exec $cmd"
}

# backend_split_strip: a small fixed-height pane ANCHORED ABOVE $target,
# for add_pane_pellicle's render strip. tmux's -b (before) + -c (explicit
# cwd, rather than embedding cd in the command) preserved exactly as the
# pre-backend-split code had it.
backend_split_strip() {
    local target="$1" dir="$2" cmd="$3" lines="$4"
    tmux split-window -v -b -l "$lines" -t "$target" -c "$dir" -P -F '#{pane_id}' "$cmd"
}

backend_capture() { tmux capture-pane -t "$1" -p 2>/dev/null; }

# backend_respawn swaps a pane's running command in place: same pane id,
# position and pane options (@cockpit_slot, @cockpit_title), new process.
backend_respawn() { tmux respawn-pane -k -t "$1" "$2"; }
backend_pane_dead() { [ "$(tmux display-message -p -t "$1" '#{pane_dead}' 2>/dev/null)" = "1" ]; }

backend_select_layout() { tmux select-layout -t "$SESSION" "$LAYOUT" >/dev/null; }
backend_select_pane() { tmux select-pane -t "$1"; }

backend_used_slots() {
    tmux list-panes -s -t "$SESSION" -F '#{@cockpit_slot}' 2>/dev/null | grep -E '^[0-9]+$' | sort -n
}
backend_get_slot() { tmux show-options -pqv -t "$1" @cockpit_slot 2>/dev/null || true; }
backend_set_slot() { tmux set-option -p -t "$1" @cockpit_slot "$2"; }
backend_set_title() { tmux set-option -p -t "$1" @cockpit_title "$2"; }
# ttmux backend needs this (no cwd exposed via its list-panes); tmux's own
# live #{b:pane_current_path} ternary already covers the fallback case, so
# there's nothing for this backend to store.
backend_set_dir() { :; }

# backend_bind_hotkeys — see bin/saturday-cockpit's original bind_hotkeys
# comment block (kept in git history/the plan doc) for the full rationale
# on every design choice below: root-table/global binds so a hardcoded
# session name can't hijack another session's keys, ##{} deferral so
# list-panes' own per-row format isn't collapsed early by run-shell,
# -s + select-window-before-select-pane for the multi-window edge case.
# The jump also sends SIGCONT to the target pane's process: a claude
# suspended with ctrl+z can't take `fg` (its pane has no shell under it,
# by design), so the key already pressed to reach it is what wakes it. A
# no-op on a process that isn't suspended.
_tmux_jump_cmd() {
    printf '%s' "p=\$(tmux list-panes -s -t '#{session_name}' -f '##{==:##{@cockpit_slot},$1}' -F '##{pane_id}' | head -1); [ -n \"\$p\" ] && { kill -CONT \"\$(tmux display-message -p -t \"\$p\" '##{pane_pid}')\" 2>/dev/null; tmux select-window -t \"\$p\" && tmux select-pane -t \"\$p\"; }; tmux resize-pane -Z"
}
backend_bind_hotkeys() {
    tmux set-option -g pane-base-index 1
    for n in 1 2 3 4 5 6 7 8 9; do
        tmux bind-key -n "M-$n" run-shell "$(_tmux_jump_cmd "$n")"
    done
    local pane=10
    for l in "${COCKPIT_HOTKEY_LETTERS[@]}"; do
        tmux bind-key -n "M-$l" run-shell "$(_tmux_jump_cmd "$pane")"
        pane=$((pane + 1))
    done
    tmux bind-key -n M-0 run-shell "tmux if-shell -F '##{window_zoomed_flag}' 'resize-pane -Z' ; tmux select-layout -t '#{session_name}' $LAYOUT"
    tmux set-hook -t "$SESSION" after-kill-pane "select-layout -t $SESSION $LAYOUT"
}

_tmux_pane_hotkey_label() {
    local expr='#{@cockpit_slot}' pane=10 l
    for l in "${COCKPIT_HOTKEY_LETTERS[@]}"; do
        expr="#{?#{==:#{@cockpit_slot},$pane},$l,$expr}"
        pane=$((pane + 1))
    done
    echo "$expr"
}

backend_apply_border_format() {
    tmux set-option -t "$SESSION" remain-on-exit on
    tmux set-window-option -t "$SESSION" pane-border-status top
    tmux set-window-option -t "$SESSION" pane-border-format \
        " $(_tmux_pane_hotkey_label):#{?@cockpit_title,#{@cockpit_title},#{b:pane_current_path}} "
}
