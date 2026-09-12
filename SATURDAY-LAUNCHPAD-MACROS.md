# Saturday + Launchpad X as a macro input surface — brainstorm, not a plan

**Status: suggestion doc.** Nothing here is decided or scoped. It exists
because a working Launchpad X MIDI driver just got built for an unrelated
reason (`efferent`'s biofeedback-actuator project) and the idea of pointing
the same physical grid at Saturday came up in passing. Treat every section
below as "worth considering," not "here's what we're building."

## What's actually confirmed vs. what's inferred

Confirmed by reading the real files this session, not assumed:

- **`efferent`'s `launchpad.go`** is a
  working, from-scratch Go driver for the Launchpad X's MIDI/SysEx
  protocol, built directly against Novation's own Programmer's Reference
  Manual (fetched and read page-by-page, not summarized). It talks to the
  ALSA rawmidi character device directly — no MIDI library — auto-locates
  the device via `/proc/asound/cards` (the card index isn't stable), enters
  Programmer mode, and lights individual grid pads via true RGB SysEx. It's
  output-only right now: nothing reads incoming Note On messages (button
  presses) yet, which is exactly the missing half a macro-input use needs.
- **`saturday`'s own `ROADMAP.md` (around line 226)** already names a
  Novation Launchpad X as "speculative and unscoped" future work — but in
  the *opposite* direction from this doc: driving RGB **output** to mirror
  the cockpit boot ritual, not reading button presses as macro **input**.
  Worth noting these aren't in conflict — the same physical device and the
  same MIDI connection carry both directions (Note On in, SysEx out), so
  whichever gets built first doesn't block the other; a real driver
  handling both would just need to own the device connection once and
  expose both directions.
- **`saturday-cockpit`'s pane-jump mechanism** (`bin/saturday-cockpit`,
  `bind_hotkeys()`) is `tmux bind-key -n M-1..M-9` / `M-<letter>` bound to
  each pane's `@cockpit_slot` user option, resolved at keypress time via a
  `tmux list-panes -f` filter, then `tmux select-pane` + `tmux resize-pane
  -Z`. This is plain shell calling tmux's own CLI — not a socket, not a
  daemon. A pad-press handler could reuse this exactly: instead of
  synthesizing a fake `Alt+N` keystroke (fragile, terminal-focus-dependent),
  a macro daemon could just run the same two tmux commands directly against
  a target `@cockpit_slot`. Concrete and low-risk.
- **`saturday-mayor` already has a working "inject text into a session"
  pathway** that isn't specific to voice: it listens on
  `/tmp/saturday-audio.sock` for a small JSON message (`Type`, `Text`,
  `Mode` — `"verbatim"` or `"expand"`, `Narrate`, `Db`, `Ts`, per
  `saturday-mayor/main.go` line ~785) and drives the same router/inject
  logic regardless of where that JSON came from. A pad-press could write
  one of these messages directly to that socket and get mayor's existing
  routing/targeting for free, rather than a macro daemon re-implementing
  "which pane is this utterance for."
- **No existing mechanism sends input to a Windows machine, checked
  directly, not assumed.** `xdotool` and `ydotool` aren't installed on this
  host. No KVM/`virt-manager`. `grep`-ing the whole `saturday` repo for
  `windows|rdp|vnc|xdotool|ydotool` turns up only prose mentions, no code.
  What **is** installed: `remmina` with its RDP and VNC plugins, and the
  underlying `libfreerdp3` client library — a real, capable RDP/VNC client
  stack already sits on this box, just not wired to anything scriptable.
- **`SATURDAY-VOICE-NATIVE.md`'s §3 already names a "PC, RTX 3080 (10GB)"**
  as part of Saturday's own planned hardware topology (hosting
  `saturday-backend`). Whether that's the same Windows box Justin means by
  "sending prerecorded inputs to windows" is **not confirmed** — worth
  asking rather than assuming, since the doc doesn't say what OS that PC
  runs.

## The shared-hardware constraint, named plainly

This would be the same one physical Launchpad X `efferent` already drives
for CO2/session-state color signals — not a second unit. It can't be lit up
green-for-calm-air and simultaneously be Saturday's macro pad at the same
moment; whichever process holds the ALSA MIDI device open owns it.
Realistic options, none chosen here:

- **Time-share by context.** Efferent's ambient signal is mostly about the
  *steady-state* color, checked/updated periodically — a macro-input daemon
  could grab the device on demand (e.g. while the cockpit has terminal
  focus) and hand it back. Needs a real handoff protocol, not assumed.
- **One process owns the device, multiplexes both jobs.** A single driver
  that both lights pads (efferent's job) and reads button presses
  (Saturday's job), with each project talking to it instead of to the raw
  device. More coordination work up front, avoids the handoff problem.
- **Buy a second unit.** A cheap grid controller (a Launchpad Mini, an
  Akai APC, even a generic MIDI pad controller) dedicated to Saturday
  alone sidesteps the whole question. Real cost, zero coordination
  complexity — worth weighing against how often the two jobs would
  actually collide in practice.

## Use case 1: fast cockpit pane switching

The concrete hook point already exists and doesn't need touching:
`saturday-cockpit`'s `@cockpit_slot` scheme. A macro daemon reading
Launchpad Note On messages could map each of the 64 grid pads (or however
many panes are realistically ever open at once) to one `@cockpit_slot`,
and on press run:

```
tmux list-panes -t <session> -f '#{==:#{@cockpit_slot},N}' -F '#{pane_id}'
tmux select-pane -t <pane_id>
tmux resize-pane -Z
```

— literally the same two-step sequence `bind_hotkeys()` already binds to
`Alt+N`. The daemon wouldn't need to know anything about tmux's internal
layout logic, just shell out the same way the existing script does. Lighting
each pad to reflect which slot it represents (or dimming/coloring by
pane state, if `saturday-watcher`'s session state were piped in) is a
natural pairing with `launchpad.go`'s existing `LightPad` — the grid becomes
a literal, at-a-glance map of the cockpit instead of a blind keypad.

## Use case 2: prerecorded input sequences to a Windows machine

This is the half with no existing bridge. Two real shapes to choose
between, neither built or spiked:

1. **A small agent on the Windows side.** A tiny process (Go compiles
   there fine) listening on the LAN for a macro-trigger message and
   replaying it locally via a Windows input-injection API (`SendInput`, or
   a thin wrapper library). Most reliable — no dependency on a remote
   desktop protocol's own input-forwarding fidelity — but is genuinely new
   code on a second OS, and needs its own auth story (this box's Tailscale
   identity, if one exists, would be the natural fit — not confirmed
   whether the Windows box is already on the same tailnet).
2. **Drive the existing Remmina/FreeRDP stack from the Linux side.**
   `libfreerdp3`'s CLI (`xfreerdp`) is already installed and *might*
   support scripted, one-shot input injection into an existing RDP
   session — genuinely unchecked this session, not confirmed either way.
   If it does, this needs zero new code on the Windows machine at all. If
   it doesn't (or only supports full interactive sessions, not scripted
   one-shot macros), option 1 is the fallback.

Worth spiking option 2 first, cheaply, before committing to writing and
deploying Windows-side code — it's a much smaller experiment (does
`xfreerdp` have a flag for this) than building a full companion agent.

## Open questions — Justin's to answer, not resolved here

- Same physical Launchpad shared with efferent, or worth buying a second
  cheap grid controller for genuine dual duty?
- Is the "PC, RTX 3080" already named in `SATURDAY-VOICE-NATIVE.md`'s
  hardware topology the Windows machine meant here, or a different box?
- Worth spiking `xfreerdp`'s scripting surface before deciding whether a
  Windows-side agent is even necessary?
- Where would the macro daemon itself live — a new small binary in
  `saturday`, a mode added to `efferent`'s existing driver, or something
  that vendors/extracts `launchpad.go`'s MIDI layer into a shared package
  once both projects actually need it? (Extracting now, before there's a
  second real consumer, would be premature — noting it as a *later* option,
  not a next step.)
