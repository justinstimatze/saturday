// saturday-stage — window choreography sidecar (Layer A: tmux).
//
// Surfaces and highlights the Claude Code session Saturday is addressing.
// Listens on a Unix socket where mayor (and any observer) connects:
//
//	commands  in  (mayor → stage):  {"type":"focus"|"restore"|"highlight", ...}
//	events    out (stage → all):    {"type":"window_activity", ...}
//
// Every connection is a bidirectional peer: stage reads command lines from
// each conn and broadcasts window_activity frames to all conns. Mayor dials
// in and sends commands (ignoring the event echo); saturday-thinking (or any
// observer) dials in and just reads the activity stream.
//
// Two layers, split along the seam Saturday already lives on:
//
//	Layer A — tmux panes/windows *inside* a terminal. Pure tmux, no Wayland.
//	          This binary. select-window / select-pane + reversible border
//	          highlight. Honest reach: it can foreground the right pane/window
//	          *within* the addressed tmux session and tint its border. It
//	          CANNOT raise that session's terminal across OS windows when each
//	          project is its own `cc-<proj>` tmux session (V0.1.5) — that's...
//	Layer B — OS windows (the terminal emulator window, browser windows).
//	          Hits Wayland's no-client-positioning wall. Decision: target
//	          Hyprland/Sway (hyprctl + wlr-foreign-toplevel), not GNOME. Lives
//	          behind the WindowSource interface as a stub here (--backend
//	          hyprland) until the compositor switch lands.
//
// Stdlib-only on purpose, matching the watcher's ethos — shells out to tmux.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Command is one instruction from mayor. PaneID is a tmux pane id (e.g. %5)
// resolved on mayor's side via inject.FindTmuxPane; the tmux backend uses it to
// locate the window/session. Conf is informational — mayor only emits focus
// after an inject has already passed its confidence gate, so by the time a
// command arrives it is confident by construction; Level lets a caller ask
// for a non-relocating highlight directly.
type Command struct {
	Type      string  `json:"type"`       // focus | restore | highlight
	SessionID string  `json:"session_id"` // CC session UUID — stage keys highlight state by this
	Project   string  `json:"project"`
	PaneID    string  `json:"pane_id"`
	Cwd       string  `json:"cwd"`
	Level     string  `json:"level"` // highlight only: active | done | dim
	Conf      float64 `json:"conf"`
	// Posture A (cockpit) modifiers on a focus command. Zoom maximizes the
	// addressed pane (resize-pane -Z); Tile gives it a proportionally larger
	// share of an even-horizontal row (salience emphasis) without reordering
	// panes. Zoom wins if both set. Both are reverted on restore from the
	// window's saved layout.
	Zoom bool `json:"zoom"`
	Tile bool `json:"tile"`
}

// Event is the monitor stream. Privacy: only emitted for panes whose tmux
// session matches the allowlist (default ^cc- — the CC session naming from
// V0.1.5), so it is CC-sessions-only and safe by construction. session_id is
// left empty for the tmux source (tmux doesn't know the CC UUID); consumers
// join Cwd → CC session via the watcher. Browser windows arrive only via the
// Hyprland backend with its own class allowlist (stubbed).
type Event struct {
	Type        string  `json:"type"` // window_activity
	Source      string  `json:"source"`
	SessionID   string  `json:"session_id,omitempty"`
	TmuxSession string  `json:"tmux_session,omitempty"`
	PaneID      string  `json:"pane_id,omitempty"`
	Cwd         string  `json:"cwd,omitempty"`
	Focused     bool    `json:"focused"`
	TS          float64 `json:"ts"`
}

// WindowSource is the backend seam. tmuxSource is Layer A (built);
// hyprlandSource is Layer B (stub) — same method set so the wire and the
// server loop don't change when the compositor switch lands.
type WindowSource interface {
	Name() string
	Focus(c Command) error
	Restore(c Command) error
	Highlight(c Command) error
	// Watch emits window_activity events via emit until the process exits.
	// allow gates which windows are observed (privacy allowlist).
	Watch(allow *regexp.Regexp, poll time.Duration, emit func(Event))
}

// ---- color register (mirrors V0.2.7 mayor stderr register) ----

func levelColor(level string) string {
	switch level {
	case "done":
		return "colour46" // green — task complete
	case "dim":
		return "colour240" // grey — de-emphasized
	default:
		return "colour51" // cyan — active / being addressed
	}
}

func levelGlyph(level string) string {
	switch level {
	case "done":
		return "✓"
	case "dim":
		return "·"
	default:
		return "●"
	}
}

// ---- tmux backend (Layer A) ----

type savedStyle struct {
	borderStatus      string // pane-border-status
	activeBorderStyle string // pane-active-border-style
}

type tmuxSource struct {
	noRelocate    bool
	tileEmphasis  float64
	tweenDuration time.Duration
	tweenTauMs    float64 // tweenTau pre-converted to ms, what smoothdampStep wants

	mu sync.Mutex
	// touched maps CC session_id → the tmux window we restyled, plus the
	// window's original styles AND its pre-modification layout string, so
	// restore reverts exactly what focus changed (border, zoom, tiling) even
	// if the active pane moved in the meantime. window_layout round-trips
	// through select-layout, so it's the faithful restore for zoom/tile.
	touched map[string]touchedWindow
}

type touchedWindow struct {
	window      string
	saved       savedStyle
	savedLayout string
	zoomed      bool // stage zoomed this pane → unzoom on restore
}

func newTmuxSource(noRelocate bool, tileEmphasis float64, tweenDuration, tweenTau time.Duration) *tmuxSource {
	return &tmuxSource{
		noRelocate:    noRelocate,
		tileEmphasis:  tileEmphasis,
		tweenDuration: tweenDuration,
		tweenTauMs:    float64(tweenTau.Milliseconds()),
		touched:       map[string]touchedWindow{},
	}
}

func (t *tmuxSource) Name() string { return "tmux" }

func tmuxOut(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	return strings.TrimSpace(string(out)), err
}

func tmuxRun(args ...string) error {
	return exec.Command("tmux", args...).Run()
}

// windowOf resolves a pane id to its window id (e.g. %5 → @3).
func windowOf(paneID string) (string, error) {
	if paneID == "" {
		return "", fmt.Errorf("empty pane id")
	}
	return tmuxOut("display-message", "-p", "-t", paneID, "#{window_id}")
}

// Focus foregrounds the addressed pane within its tmux session and applies
// the active highlight. select-window/select-pane only move focus *inside*
// that session — raising the session's terminal across OS windows is Layer B.
func (t *tmuxSource) Focus(c Command) error {
	if c.PaneID == "" {
		return fmt.Errorf("focus: no pane id")
	}
	if !t.noRelocate {
		// select-window before select-pane: the window must be current for the
		// pane selection to be the one the user lands on.
		_ = tmuxRun("select-window", "-t", c.PaneID)
		_ = tmuxRun("select-pane", "-t", c.PaneID)
	}
	hc := c
	hc.Level = "active"
	if err := t.Highlight(hc); err != nil { // also captures saved layout on first touch
		return err
	}
	// Posture A emphasis. Zoom is the binary extreme (maximize, hide others);
	// Tile is the graded version (bigger share of an even row). Mutually
	// exclusive — zoom wins.
	switch {
	case c.Zoom:
		return t.zoomPane(c.SessionID, c.PaneID)
	case c.Tile:
		return t.proportionalTile(c.PaneID)
	}
	return nil
}

// zoomPane maximizes the addressed pane (resize-pane -Z), recording that stage
// zoomed it so restore can toggle back. No-op if the window is already zoomed
// (avoids un-zooming a user's manual zoom).
func (t *tmuxSource) zoomPane(sessionID, paneID string) error {
	win, err := windowOf(paneID)
	if err != nil {
		return err
	}
	if z, _ := tmuxOut("display-message", "-p", "-t", win, "#{window_zoomed_flag}"); z == "1" {
		return nil
	}
	if err := tmuxRun("resize-pane", "-Z", "-t", paneID); err != nil {
		return err
	}
	t.mu.Lock()
	if tw, ok := t.touched[sessionID]; ok {
		tw.zoomed = true
		t.touched[sessionID] = tw
	}
	t.mu.Unlock()
	return nil
}

// paneGeom is one pane's absolute position/size within its window, as
// reported by list-panes. tmux doesn't expose its split tree as pane
// geometry directly, so proportionalTile infers split-family membership
// from this instead of parsing tmux's own layout-string grammar.
type paneGeom struct {
	id            string
	left, top     int
	width, height int
}

// parsePaneGeom parses list-panes -F "#{pane_id} #{pane_left} #{pane_top}
// #{pane_width} #{pane_height}" output (one pane per line) into paneGeom
// values, skipping any malformed line rather than erroring the whole call.
func parsePaneGeom(out string) []paneGeom {
	var panes []paneGeom
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		left, err1 := strconv.Atoi(f[1])
		top, err2 := strconv.Atoi(f[2])
		width, err3 := strconv.Atoi(f[3])
		height, err4 := strconv.Atoi(f[4])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		panes = append(panes, paneGeom{id: f[0], left: left, top: top, width: width, height: height})
	}
	return panes
}

func paneByID(panes []paneGeom, id string) (paneGeom, bool) {
	for _, p := range panes {
		if p.id == id {
			return p, true
		}
	}
	return paneGeom{}, false
}

// siblingGroups finds the addressed pane's row-mates (panes sharing its
// top+height — the horizontal split-family a -x resize would move) and
// column-mates (panes sharing its left+width — the vertical split-family a
// -y resize would move). Pure: works entirely off already-parsed geometry,
// no tmux calls, so it's directly fixture-testable. Each group includes the
// addressed pane itself. Either group can come back with just the addressed
// pane alone (e.g. a full-width footer pane has no row-mates) — that's a
// legitimate per-axis "nothing to tile against", not an error.
func siblingGroups(panes []paneGeom, addressedID string) (rowMates, colMates []paneGeom) {
	addressed, ok := paneByID(panes, addressedID)
	if !ok {
		return nil, nil
	}
	for _, p := range panes {
		if p.top == addressed.top && p.height == addressed.height {
			rowMates = append(rowMates, p)
		}
		if p.left == addressed.left && p.width == addressed.width {
			colMates = append(colMates, p)
		}
	}
	return rowMates, colMates
}

// tileAxisTargets computes each non-skipped pane's target size along one
// axis: the addressed pane gets a tileEmphasis:1 share against every other
// pane in group, the remainder split evenly among the rest — same math as
// the original two-tier proportionalTile, now scoped to a real
// split-family group instead of the whole window, and fed the group's own
// summed size rather than the raw window dimension (numerically the same
// on a group that spans the full window, correct in general — e.g. a
// future sidebar-plus-grid layout where a row only spans part of the
// window). One pane is always left unresized so tmux absorbs the
// integer-division remainder into it — never the addressed pane, so its
// emphasis always lands explicitly. nil (not an empty map) means "nothing
// to tile on this axis" (fewer than 2 panes, or zero total size).
func tileAxisTargets(group []paneGeom, addressedID string, tileEmphasis float64, sizeOf func(paneGeom) int) map[string]int {
	if len(group) < 2 {
		return nil
	}
	total := 0
	for _, p := range group {
		total += sizeOf(p)
	}
	if total <= 0 {
		return nil
	}
	skip := len(group) - 1
	if group[skip].id == addressedID {
		skip = len(group) - 2
	}
	denom := tileEmphasis + float64(len(group)-1)
	target := int(tileEmphasis / denom * float64(total))
	rest := (total - target) / (len(group) - 1)
	targets := make(map[string]int, len(group)-1)
	for i, p := range group {
		if i == skip {
			continue
		}
		if p.id == addressedID {
			targets[p.id] = target
		} else {
			targets[p.id] = rest
		}
	}
	return targets
}

// proportionalTile emphasizes the addressed pane within its real row and
// column split-families instead of forcing the window into a single row.
// tmux resize-pane already only redistributes space among a pane's
// immediate split-family siblings — that's the mechanism the old
// even-horizontal-forcing code was fighting — so once the layout isn't
// flattened first, resize-pane naturally respects whatever real grid or
// nested arrangement (e.g. a pellicle status-strip pair) is already there.
// A full per-session weight vector can drive this later when mayor pushes
// one; two-tier (addressed vs. its own siblings) for now.
func (t *tmuxSource) proportionalTile(paneID string) error {
	win, err := windowOf(paneID)
	if err != nil {
		return err
	}
	// Don't fight a user's manual zoom: list-panes reports a zoomed pane at
	// full-window geometry while every other pane keeps its pre-zoom
	// logical position (confirmed live — a real cockpit session had a pane
	// mid-zoom while this was being designed), which would corrupt the
	// grouping below and make resizing the other panes unreliable. Same
	// zoomed-flag check zoomPane/Restore already use.
	if z, _ := tmuxOut("display-message", "-p", "-t", win, "#{window_zoomed_flag}"); z == "1" {
		return nil
	}
	out, err := tmuxOut("list-panes", "-t", win, "-F",
		"#{pane_id} #{pane_left} #{pane_top} #{pane_width} #{pane_height}")
	if err != nil {
		return err
	}
	panes := parsePaneGeom(out)
	rowMates, colMates := siblingGroups(panes, paneID)

	var tweens []paneTween
	addTweens := func(group []paneGeom, axis string, sizeOf func(paneGeom) int) {
		for id, target := range tileAxisTargets(group, paneID, t.tileEmphasis, sizeOf) {
			p, ok := paneByID(panes, id)
			if !ok {
				continue
			}
			tweens = append(tweens, paneTween{pane: id, axis: axis, cur: float64(sizeOf(p)), target: float64(target)})
		}
	}
	addTweens(rowMates, "x", func(p paneGeom) int { return p.width })
	addTweens(colMates, "y", func(p paneGeom) int { return p.height })

	if len(tweens) == 0 {
		return nil // nothing to tile against on either axis
	}
	fmt.Fprintf(os.Stderr, "\033[2m  tile: win=%s addressed=%s axes=%d emphasis=%.1f\033[0m\n",
		win, paneID, len(tweens), t.tileEmphasis)
	return t.animateResizes(tweens)
}

// paneTween is one pane-axis's animated resize: its current size sliding
// toward its target.
type paneTween struct {
	pane   string
	axis   string // "x" or "y" — which resize-pane flag drives this
	cur    float64
	target float64
}

// smoothdampStep is jonaburg/tmux-animated's own pane-resize formula
// (animation.c:212-224 on its `animations` branch — fetched and verified
// against the real source, not recalled from memory), ported to Go rather
// than invented fresh: pure exponential decay toward target. It never
// exactly reaches target in finite time — animateResizes hard-snaps on its
// own duration budget rather than looping on convergence.
func smoothdampStep(pos, target, tauMs, dtMs float64) float64 {
	if tauMs <= 0 {
		tauMs = 1 // matches the real source's own guard
	}
	alpha := 1 - math.Exp(-dtMs/tauMs)
	return pos + alpha*(target-pos)
}

// tileTweenFrameInterval bounds how often animateResizes can spawn a
// resize-pane subprocess. Not a flag: nothing in this design needs it
// operator-tunable, and exposing it would grow the flag surface for its
// own sake.
const tileTweenFrameInterval = 30 * time.Millisecond

// tmuxRunFn is the seam animateResizes calls through instead of tmuxRun
// directly, so tests can substitute a recording fake and assert call
// counts/order without shelling to a real tmux.
var tmuxRunFn = tmuxRun

// animateResizes steps every tween's current size toward its target via
// smoothdampStep on one shared ticker (not one goroutine per pane — keeps
// subprocess spawns bounded and batched per tick rather than staggered),
// issuing resize-pane only when a pane's rounded size actually changes
// since the last tick (skips a spawn when sub-cell motion hasn't crossed
// an integer boundary yet). Runs for a fixed wall-clock budget
// (t.tweenDuration), then force-snaps every pane to its exact integer
// target — smoothdamp is asymptotic and never arrives on its own.
func (t *tmuxSource) animateResizes(tweens []paneTween) error {
	if t.tweenDuration <= 0 {
		// Explicit fast path, not an implicit degenerate case: smoothdamp
		// has no convergence point to fall back on, so a zero-duration tick
		// loop wouldn't do the right thing on its own. Matches the old
		// instant-jump behavior exactly.
		for _, tw := range tweens {
			target := int(math.Round(tw.target))
			if err := tmuxRunFn("resize-pane", "-t", tw.pane, "-"+tw.axis, strconv.Itoa(target)); err != nil {
				fmt.Fprintf(os.Stderr, "\033[2m  tile: resize-pane -t %s -%s %d: %v\033[0m\n", tw.pane, tw.axis, target, err)
			}
		}
		return nil
	}

	last := make([]int, len(tweens))
	for i, tw := range tweens {
		last[i] = int(math.Round(tw.cur))
	}

	deadline := time.Now().Add(t.tweenDuration)
	tick := time.NewTicker(tileTweenFrameInterval)
	defer tick.Stop()
	prev := time.Now()
	for now := range tick.C {
		dtMs := float64(now.Sub(prev).Milliseconds())
		prev = now
		for i := range tweens {
			tweens[i].cur = smoothdampStep(tweens[i].cur, tweens[i].target, t.tweenTauMs, dtMs)
			rounded := int(math.Round(tweens[i].cur))
			if rounded == last[i] {
				continue
			}
			last[i] = rounded
			if err := tmuxRunFn("resize-pane", "-t", tweens[i].pane, "-"+tweens[i].axis, strconv.Itoa(rounded)); err != nil {
				fmt.Fprintf(os.Stderr, "\033[2m  tile: resize-pane -t %s -%s %d: %v\033[0m\n", tweens[i].pane, tweens[i].axis, rounded, err)
			}
		}
		if !now.Before(deadline) {
			break
		}
	}
	for i, tw := range tweens {
		target := int(math.Round(tw.target))
		if last[i] == target {
			continue
		}
		if err := tmuxRunFn("resize-pane", "-t", tw.pane, "-"+tw.axis, strconv.Itoa(target)); err != nil {
			fmt.Fprintf(os.Stderr, "\033[2m  tile: resize-pane -t %s -%s %d: %v\033[0m\n", tw.pane, tw.axis, target, err)
		}
	}
	return nil
}

// Highlight tints the window's active-pane border and titles the pane, saving
// the window's prior styles on first touch so Restore can revert cleanly
// (same save/restore discipline as mayor's blinkenlights status-right).
func (t *tmuxSource) Highlight(c Command) error {
	win, err := windowOf(c.PaneID)
	if err != nil {
		return fmt.Errorf("highlight: resolve window: %w", err)
	}
	color := levelColor(c.Level)

	t.mu.Lock()
	if _, ok := t.touched[c.SessionID]; !ok {
		bs, _ := tmuxOut("show-options", "-wqv", "-t", win, "pane-border-status")
		abs, _ := tmuxOut("show-options", "-wqv", "-t", win, "pane-active-border-style")
		layout, _ := tmuxOut("display-message", "-p", "-t", win, "#{window_layout}")
		t.touched[c.SessionID] = touchedWindow{
			window:      win,
			saved:       savedStyle{borderStatus: bs, activeBorderStyle: abs},
			savedLayout: layout,
		}
	}
	t.mu.Unlock()

	_ = tmuxRun("set-option", "-w", "-t", win, "pane-border-status", "top")
	_ = tmuxRun("set-option", "-w", "-t", win, "pane-active-border-style", "fg="+color+",bold")
	title := fmt.Sprintf("%s saturday → %s", levelGlyph(c.Level), c.Project)
	_ = tmuxRun("select-pane", "-t", c.PaneID, "-T", title)
	return nil
}

// Restore reverts the window styles stage changed for this session. Keyed by
// session_id so mayor's restore needs only the id. No-op (and no error) for a
// session stage never highlighted — covers direct-write/headless injects and
// TTL-expiry teardown that never had a live pane.
func (t *tmuxSource) Restore(c Command) error {
	t.mu.Lock()
	tw, ok := t.touched[c.SessionID]
	if ok {
		delete(t.touched, c.SessionID)
	}
	t.mu.Unlock()
	if !ok {
		return nil
	}
	// Undo geometry first: unzoom if we zoomed, then re-apply the pre-touch
	// layout (round-trips through select-layout, faithfully reverting zoom and
	// proportional tiling alike).
	if tw.zoomed {
		if z, _ := tmuxOut("display-message", "-p", "-t", tw.window, "#{window_zoomed_flag}"); z == "1" {
			_ = tmuxRun("resize-pane", "-Z", "-t", tw.window)
		}
	}
	if tw.savedLayout != "" {
		_ = tmuxRun("select-layout", "-t", tw.window, tw.savedLayout)
	}
	if tw.saved.borderStatus == "" {
		_ = tmuxRun("set-option", "-w", "-t", tw.window, "-u", "pane-border-status")
	} else {
		_ = tmuxRun("set-option", "-w", "-t", tw.window, "pane-border-status", tw.saved.borderStatus)
	}
	if tw.saved.activeBorderStyle == "" {
		_ = tmuxRun("set-option", "-w", "-t", tw.window, "-u", "pane-active-border-style")
	} else {
		_ = tmuxRun("set-option", "-w", "-t", tw.window, "pane-active-border-style", tw.saved.activeBorderStyle)
	}
	return nil
}

// Watch polls tmux for the user-focused pane and emits a window_activity
// event when it changes. "Focused" = the active pane of the active window of
// the most-recently-active attached client's session. Only allowlisted tmux
// sessions are observed; everything else is never inspected.
func (t *tmuxSource) Watch(allow *regexp.Regexp, poll time.Duration, emit func(Event)) {
	tick := time.NewTicker(poll)
	defer tick.Stop()
	var last string // pane id last reported focused, to emit only on change
	for range tick.C {
		front := frontSession()
		if front == "" {
			continue
		}
		// Active pane of the active window of the front session.
		out, err := tmuxOut("list-panes", "-t", front, "-F",
			"#{session_name}\t#{pane_id}\t#{pane_active}\t#{window_active}\t#{pane_current_path}")
		if err != nil {
			continue
		}
		var sess, pane, cwd string
		for _, line := range strings.Split(out, "\n") {
			f := strings.SplitN(line, "\t", 5)
			if len(f) < 5 {
				continue
			}
			if f[2] == "1" && f[3] == "1" {
				sess, pane, cwd = f[0], f[1], f[4]
				break
			}
		}
		if pane == "" || pane == last {
			continue
		}
		if allow != nil && !allow.MatchString(sess) {
			last = pane // remember so we don't re-check every tick, but emit nothing
			continue
		}
		last = pane
		emit(Event{
			Type:        "window_activity",
			Source:      "tmux",
			TmuxSession: sess,
			PaneID:      pane,
			Cwd:         cwd,
			Focused:     true,
			TS:          float64(time.Now().UnixNano()) / 1e9,
		})
	}
}

// frontSession returns the tmux session of the most-recently-active attached
// client — the best stdlib proxy for "what the user is looking at".
func frontSession() string {
	out, err := tmuxOut("list-clients", "-F", "#{client_activity}\t#{client_session}")
	if err != nil || out == "" {
		return ""
	}
	var bestTS int64 = -1
	var best string
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 2)
		if len(f) < 2 {
			continue
		}
		var ts int64
		fmt.Sscanf(f[0], "%d", &ts)
		if ts > bestTS {
			bestTS, best = ts, f[1]
		}
	}
	return best
}

// ---- hyprland backend (Layer B) — stub until the compositor switch ----

type hyprlandSource struct{ once sync.Once }

func (h *hyprlandSource) Name() string { return "hyprland" }

func (h *hyprlandSource) notImpl(op string) error {
	h.once.Do(func() {
		fmt.Fprintln(os.Stderr, "\033[2msaturday-stage: hyprland backend is a stub — Layer B (OS-window move/highlight) lands with the Hyprland/Sway switch; use --backend tmux for now\033[0m")
	})
	return fmt.Errorf("hyprland backend not implemented: %s", op)
}

func (h *hyprlandSource) Focus(c Command) error     { return h.notImpl("focus") }
func (h *hyprlandSource) Restore(c Command) error   { return h.notImpl("restore") }
func (h *hyprlandSource) Highlight(c Command) error { return h.notImpl("highlight") }
func (h *hyprlandSource) Watch(_ *regexp.Regexp, _ time.Duration, _ func(Event)) {
	// Real impl: subscribe to the Hyprland IPC event socket (activewindow,
	// openwindow, closewindow) — a push feed, no polling — and apply the
	// browser-class allowlist before emitting. Stubbed for now.
}

// ---- server: one bidirectional socket, fan-out events, fan-in commands ----

type stage struct {
	src WindowSource

	connsMu sync.Mutex
	conns   map[net.Conn]struct{}
}

func (s *stage) broadcast(ev Event) {
	b, _ := json.Marshal(ev)
	b = append(b, '\n')
	s.connsMu.Lock()
	for c := range s.conns {
		if _, err := c.Write(b); err != nil {
			c.Close()
			delete(s.conns, c)
		}
	}
	s.connsMu.Unlock()
}

func (s *stage) handleConn(conn net.Conn) {
	s.connsMu.Lock()
	s.conns[conn] = struct{}{}
	s.connsMu.Unlock()
	defer func() {
		s.connsMu.Lock()
		delete(s.conns, conn)
		s.connsMu.Unlock()
		conn.Close()
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c Command
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			continue // observers send nothing; ignore garbage
		}
		s.dispatch(c)
	}
}

func (s *stage) dispatch(c Command) {
	var err error
	switch c.Type {
	case "focus":
		err = s.src.Focus(c)
		logCmd("focus", c, err)
	case "restore":
		err = s.src.Restore(c)
		// restore is a frequent no-op (non-tmux injects) — only log real errors
		if err != nil {
			logCmd("restore", c, err)
		}
	case "highlight":
		err = s.src.Highlight(c)
		logCmd("highlight", c, err)
	default:
		// ignore unknown types (forward-compat with the audio sock's ethos)
	}
}

func logCmd(op string, c Command, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "\033[2m✗ %s %s: %v\033[0m\n", op, c.Project, err)
		return
	}
	fmt.Fprintf(os.Stderr, "\033[36m● %s → %s\033[0m \033[2m(%s)\033[0m\n", op, c.Project, c.PaneID)
}

func main() {
	sockPath := flag.String("sock", "/tmp/saturday-stage.sock", "Unix socket: mayor connects to send focus/restore/highlight commands; observers connect to read the window_activity stream")
	backend := flag.String("backend", "tmux", "window backend: tmux (Layer A, built) | hyprland (Layer B, stub until the compositor switch)")
	allowRe := flag.String("allow", "^cc-", "regex over tmux session names — only matching sessions are observed in the activity stream. Default ^cc- = CC sessions only (privacy-safe by construction). Empty = observe all.")
	poll := flag.Duration("poll", time.Second, "tmux activity poll interval")
	noRelocate := flag.Bool("no-relocate", false, "highlight only — never select-window/select-pane (no focus motion, just border tint)")
	tileEmphasis := flag.Float64("tile-emphasis", 3.0, "Posture A: on a tile focus, the addressed pane's width/height share relative to each sibling in its own row/column (3 = ~3x larger). 1 = even.")
	tileTweenDuration := flag.Duration("tile-tween-duration", 180*time.Millisecond, "Posture A: total animation budget for a tile-focus resize (smoothdamp easing, ported from jonaburg/tmux-animated). 0 disables tweening (instant resize).")
	tileTweenTau := flag.Duration("tile-tween-tau", 50*time.Millisecond, "Posture A: smoothdamp time constant for tile-focus resizes — shapes the curve's inertia.")
	flag.Parse()

	var src WindowSource
	switch *backend {
	case "tmux":
		src = newTmuxSource(*noRelocate, *tileEmphasis, *tileTweenDuration, *tileTweenTau)
	case "hyprland":
		src = &hyprlandSource{}
	default:
		fmt.Fprintf(os.Stderr, "unknown --backend %q (want tmux|hyprland)\n", *backend)
		os.Exit(2)
	}

	var allow *regexp.Regexp
	if *allowRe != "" {
		re, err := regexp.Compile(*allowRe)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad --allow regex: %v\n", err)
			os.Exit(2)
		}
		allow = re
	}

	_ = os.Remove(*sockPath)
	ln, err := net.Listen("unix", *sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", *sockPath, err)
		os.Exit(1)
	}
	defer os.Remove(*sockPath)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigs
		os.Remove(*sockPath)
		os.Exit(0)
	}()

	s := &stage{src: src, conns: map[net.Conn]struct{}{}}

	go src.Watch(allow, *poll, s.broadcast)

	fmt.Fprintf(os.Stderr, "\033[1;32m[ready] saturday-stage — backend=%s allow=%q listening on %s\033[0m\n", src.Name(), *allowRe, *sockPath)
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "accept:", err)
			continue
		}
		go s.handleConn(conn)
	}
}
