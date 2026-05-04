// saturday-thinking — TUI render of saturday-mayor's cognitive state.
//
// Subscribes to mayor's state socket (line-delimited JSON snapshots),
// renders a bordered-cards FUI to its stdout via bubbletea + lipgloss.
// Designed to run in saturday-stack's top-right pane.
//
// Aesthetic register pulls from lucida/design-references.md — Coleran's
// pragmatic-futurism (engineering precision over Iron Man cosplay), edge
// telemetry density (Tony's monitor corners), Black Mirror's concentric
// ring nav. We compose bubbletea/lipgloss + a few hand-rolled braille
// primitives. There's no sci-fi HUD library for Go yet (verified via
// awesome-tuis 2026-05-03); shape these primitives so they can be
// extracted into one later — see memory/project_fui_go_tui_library.md.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// MayorState mirrors saturday-mayor's wire format (v=1).
type MayorState struct {
	V       int               `json:"v"`
	TS      float64           `json:"ts"`
	UptimeS float64           `json:"uptime_s"`
	State   string            `json:"state"`
	Target  string            `json:"target"`
	Tracked []TrackedInject   `json:"tracked"`
	Recent  []RecentUtterance `json:"recent"`
	Turns   int               `json:"turns"`
	// V0.2.7: rolling mic dBFS samples (oldest → newest), 5 Hz from
	// saturday-audio. -90 = silence/muted, 0 = full-scale.
	Rms []float64 `json:"rms"`
}

type TrackedInject struct {
	SID   string  `json:"sid"`
	Proj  string  `json:"proj"`
	AgeS  float64 `json:"age_s"`
	Block string  `json:"block"`
}

type RecentUtterance struct {
	TS    float64 `json:"ts"`
	Text  string  `json:"text"`
	Mode  string  `json:"mode"`
	Route string  `json:"route"`
	Conf  float64 `json:"conf"`
}

// stateMirror caches the latest snapshot for the bubbletea program. The
// socket reader goroutine writes; Update() messages read indirectly via
// stateMsg.
type stateMirror struct {
	mu        sync.Mutex
	cur       MayorState
	connected bool
}

func (s *stateMirror) Set(v MayorState) {
	s.mu.Lock()
	s.cur = v
	s.connected = true
	s.mu.Unlock()
}

func (s *stateMirror) SetDisconnected() {
	s.mu.Lock()
	s.connected = false
	s.mu.Unlock()
}

func (s *stateMirror) Snapshot() (MayorState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, s.connected
}

// connectLoop dials the mayor state socket with exponential backoff and
// drains line-delimited JSON frames into the mirror. Pushes a stateMsg
// to the bubbletea program on every frame so the View re-renders.
func connectLoop(sockPath string, m *stateMirror, p *tea.Program, quit <-chan struct{}) {
	backoff := 250 * time.Millisecond
	const maxBackoff = 2 * time.Second
	for {
		select {
		case <-quit:
			return
		default:
		}
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			m.SetDisconnected()
			p.Send(stateMsg{})
			select {
			case <-quit:
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = 250 * time.Millisecond
		readFrames(conn, m, p, quit)
		conn.Close()
		m.SetDisconnected()
		p.Send(stateMsg{})
	}
}

func readFrames(conn net.Conn, m *stateMirror, p *tea.Program, quit <-chan struct{}) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		select {
		case <-quit:
			return
		default:
		}
		var snap MayorState
		if err := json.Unmarshal(sc.Bytes(), &snap); err != nil {
			continue
		}
		m.Set(snap)
		p.Send(stateMsg{})
	}
}

// --- bubbletea program ---

type stateMsg struct{}
type tickMsg struct{}

type model struct {
	mirror *stateMirror
	frame  uint64
	width  int
	height int
}

func (m model) Init() tea.Cmd {
	return tickCmd(200 * time.Millisecond)
}

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	case stateMsg:
		return m, nil // re-render on next tick
	case tickMsg:
		m.frame++
		s, _ := m.mirror.Snapshot()
		next := 200 * time.Millisecond
		if s.State != "" && s.State != "idle" {
			next = 100 * time.Millisecond
		}
		return m, tickCmd(next)
	}
	return m, nil
}

// --- styling ---

var (
	colorBaseline = lipgloss.Color("240")
	colorDim      = lipgloss.Color("245")
	colorMuted    = lipgloss.Color("250")
	colorCyan     = lipgloss.Color("51")
	colorCyanDim  = lipgloss.Color("37")
	colorGreen    = lipgloss.Color("46")
	colorYellow   = lipgloss.Color("220")
	colorRed      = lipgloss.Color("196")

	dimText  = lipgloss.NewStyle().Foreground(colorDim)
	mutedTxt = lipgloss.NewStyle().Foreground(colorMuted)
	bri      = lipgloss.NewStyle().Bold(true).Foreground(colorCyan)
	briGreen = lipgloss.NewStyle().Bold(true).Foreground(colorGreen)
	briRed   = lipgloss.NewStyle().Bold(true).Foreground(colorRed)
	briYell  = lipgloss.NewStyle().Bold(true).Foreground(colorYellow)
)

// card returns a bordered, padded box with the given title in the top
// border. activeColor sets the border color when active is true; otherwise
// the border is dim grey.
func card(title, body string, active bool, activeColor lipgloss.Color, w int) string {
	border := colorBaseline
	if active {
		border = activeColor
	}
	titleStyle := lipgloss.NewStyle().
		Foreground(border).
		Padding(0, 1)
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(w)
	if title != "" {
		// Render title as the first line of the box body, with a thin
		// underline rule. Avoids the lipgloss "title-in-border" trick
		// which is finicky at narrow widths.
		body = titleStyle.Render(title) + "\n" + body
	}
	return style.Render(body)
}

func fmtUptime(secs float64) string {
	s := int(secs)
	h := s / 3600
	m := (s % 3600) / 60
	sec := s % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, sec)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
}

func fmtTUptime(secs float64) string {
	s := int(secs)
	h := s / 3600
	m := (s % 3600) / 60
	sec := s % 60
	return fmt.Sprintf("T+%d:%02d:%02d", h, m, sec)
}

// kv renders a "key   value" row with aligned key column.
func kv(key, val string, valStyle lipgloss.Style) string {
	return dimText.Render(fmt.Sprintf("%-7s ", key)) + valStyle.Render(val)
}

// --- card renderers ---

func statusCard(s MayorState, connected bool, frame uint64, w int) string {
	stateLabel := s.State
	if stateLabel == "" {
		stateLabel = "idle"
	}
	stateStyle := mutedTxt
	switch s.State {
	case "injecting":
		stateStyle = briYell
	case "expanding", "routing", "hearing":
		stateStyle = bri
	}
	target := s.Target
	if target == "" {
		target = "—"
	}
	rate := 0.0
	if s.UptimeS > 60 {
		rate = float64(s.Turns) / (s.UptimeS / 3600)
	}
	connDot := bri.Render("●")
	if !connected {
		connDot = briRed.Render("●")
	}
	header := fmt.Sprintf("%s %s   %s",
		dimText.Render(fmtTUptime(s.UptimeS)),
		bri.Render("SATURDAY"),
		connDot,
	)
	// mic spark — width fits inside the card minus "mic " label and dB readout.
	sparkW := maxInt(w-12, 8)
	spark := rmsSpark(s.Rms, sparkW)
	sparkStyle := dimText
	dbStr := "  ─  "
	if rmsActive(s.Rms) {
		sparkStyle = briGreen
	}
	if len(s.Rms) > 0 {
		last := s.Rms[len(s.Rms)-1]
		if last <= -89 {
			dbStr = " mute"
		} else {
			dbStr = fmt.Sprintf("%4.0fdB", last)
		}
	}
	micLine := dimText.Render("mic ") + sparkStyle.Render(spark) + " " + dimText.Render(dbStr)
	body := strings.Join([]string{
		header,
		dimText.Render(strings.Repeat("─", maxInt(w-2, 8))),
		kv("state", stateLabel, stateStyle),
		kv("target", target, mutedTxt),
		kv("uptime", fmt.Sprintf("%s · %.1f/hr · %d utt", fmtUptime(s.UptimeS), rate, s.Turns), mutedTxt),
		micLine,
	}, "\n")
	active := connected && s.State != "" && s.State != "idle"
	return card("", body, active, colorCyan, w)
}

func pipelineCard(s MayorState, frame uint64, w int) string {
	stages := []string{"heard", "route", "expand", "inject"}
	current := -1
	switch s.State {
	case "hearing":
		current = 0
	case "routing":
		current = 1
	case "expanding":
		current = 2
	case "injecting":
		current = 3
	}
	// Render two rows: stage labels with separators, and dots underneath.
	labels := make([]string, len(stages))
	dots := make([]string, len(stages))
	for i, name := range stages {
		nameStyle := dimText
		dotStyle := dimText
		dot := "◌"
		if i < current {
			nameStyle = mutedTxt
			dotStyle = briGreen
			dot = "◉"
		} else if i == current {
			nameStyle = bri
			dotStyle = bri
			dot = "◉"
		}
		labels[i] = nameStyle.Render(name)
		// Center-align the dot under each label (simple — assume each
		// label is width 6 and place dot in middle).
		dots[i] = dotStyle.Render(dot)
	}
	sep := dimText.Render(" ▸ ")
	dotSep := strings.Repeat(" ", 5)
	body := dimText.Render("pipeline") + "\n" +
		dimText.Render(strings.Repeat("─", maxInt(w-2, 8))) + "\n" +
		strings.Join(labels, sep) + "\n" +
		"  " + strings.Join(dots, dotSep)
	return card("", body, current >= 0, colorCyan, w)
}

func trackedCard(s MayorState, frame uint64, w int) string {
	body := dimText.Render("active injects")
	body += "\n" + dimText.Render(strings.Repeat("─", maxInt(w-2, 8)))
	if len(s.Tracked) == 0 {
		body += "\n" + dimText.Render("· no injects in flight")
	} else {
		for _, t := range s.Tracked {
			bar := brailleBar(t.AgeS, 60.0, 8)
			blockStyle := dimText
			switch t.Block {
			case "text":
				blockStyle = briGreen
			case "running":
				blockStyle = bri
			}
			line := fmt.Sprintf("%s %s  %s  %s  %s",
				dimText.Render("·"),
				mutedTxt.Render(padRight(t.Proj, 9)),
				lipgloss.NewStyle().Foreground(colorCyanDim).Render(bar),
				dimText.Render(fmt.Sprintf("%4.0fs", t.AgeS)),
				blockStyle.Render(t.Block),
			)
			body += "\n" + line
		}
	}
	return card("", body, len(s.Tracked) > 0, colorCyan, w)
}

func recentCard(s MayorState, w int) string {
	body := dimText.Render("recent")
	body += "\n" + dimText.Render(strings.Repeat("─", maxInt(w-2, 8)))
	if len(s.Recent) == 0 {
		body += "\n" + dimText.Render("· none")
	} else {
		// Show last 3 utterances, newest at top.
		n := len(s.Recent)
		start := n - 3
		if start < 0 {
			start = 0
		}
		for i := n - 1; i >= start; i-- {
			u := s.Recent[i]
			ts := time.Unix(int64(u.TS), 0)
			preview := u.Text
			if len(preview) > w-12 {
				preview = preview[:maxInt(w-15, 8)] + "…"
			}
			confTxt := ""
			if u.Conf > 0 {
				confTxt = fmt.Sprintf(" .%02d", int(u.Conf*100))
			}
			line1 := dimText.Render(ts.Format("15:04:05")) + "  " + mutedTxt.Render(fmt.Sprintf("%q", preview))
			line2 := dimText.Render("    → ") + mutedTxt.Render(fmt.Sprintf("%s · %s%s", u.Route, u.Mode, confTxt))
			body += "\n" + line1 + "\n" + line2
		}
	}
	return card("", body, false, colorCyan, w)
}

// --- helpers ---

// rmsSpark renders dBFS samples as a unicode sparkline. -60 dBFS clamps
// to lowest bar; 0 dBFS clamps to highest. Pads with leading spaces if
// the ring isn't full yet so the bar always renders right-aligned to
// width. Last sample is the most recent (right edge).
func rmsSpark(samples []float64, width int) string {
	bars := []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
	out := make([]rune, 0, width)
	// Right-align: take the last `width` samples; pad the front if shorter.
	start := 0
	if len(samples) > width {
		start = len(samples) - width
	}
	pad := width - (len(samples) - start)
	for i := 0; i < pad; i++ {
		out = append(out, ' ')
	}
	for _, db := range samples[start:] {
		// Map -60..0 dBFS → 0..(len(bars)-1).
		const floor = -60.0
		if db < floor {
			db = floor
		}
		if db > 0 {
			db = 0
		}
		idx := int((db - floor) / -floor * float64(len(bars)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(bars) {
			idx = len(bars) - 1
		}
		out = append(out, bars[idx])
	}
	return string(out)
}

// rmsActive reports whether the most recent samples show non-silent audio
// (above -55 dBFS, the noise floor). Used to color the spark green when
// the mic is hot, dim when quiet/muted.
func rmsActive(samples []float64) bool {
	if len(samples) == 0 {
		return false
	}
	// Look at last 3 samples (≈600ms) to avoid flickering.
	n := len(samples)
	start := n - 3
	if start < 0 {
		start = 0
	}
	for _, db := range samples[start:] {
		if db > -55.0 {
			return true
		}
	}
	return false
}

func brailleBar(v, cap float64, width int) string {
	if cap <= 0 {
		cap = 1
	}
	frac := v / cap
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	full := int(frac * float64(width))
	rem := frac*float64(width) - float64(full)
	out := make([]rune, 0, width)
	for i := 0; i < full && i < width; i++ {
		out = append(out, '⣿')
	}
	if full < width {
		partial := []rune{' ', '⠆', '⠶', '⡶', '⣶', '⣷', '⣿'}
		idx := int(rem * float64(len(partial)-1))
		out = append(out, partial[idx])
	}
	for len(out) < width {
		out = append(out, ' ')
	}
	return string(out)
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- View ---

func (m model) View() string {
	w := m.width
	if w <= 0 {
		w = 40
	}
	cardW := w - 2 // border
	if cardW < 24 {
		cardW = 24
	}
	if cardW > 60 {
		cardW = 60
	}
	s, connected := m.mirror.Snapshot()

	if !connected && s.UptimeS == 0 {
		// Connecting placeholder.
		spinFrames := []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}
		ch := spinFrames[m.frame%uint64(len(spinFrames))]
		return "\n  " + bri.Render(string(ch)) + "  " + dimText.Render("connecting to saturday-mayor…") + "\n"
	}

	parts := []string{
		statusCard(s, connected, m.frame, cardW),
		pipelineCard(s, m.frame, cardW),
		trackedCard(s, m.frame, cardW),
		recentCard(s, cardW),
	}
	return strings.Join(parts, "\n")
}

// --- main ---

func main() {
	sockPath := flag.String("state-sock", "/tmp/saturday-mayor-state.sock", "path to saturday-mayor state socket")
	flag.Parse()

	mirror := &stateMirror{}
	m := model{mirror: mirror}
	p := tea.NewProgram(m, tea.WithAltScreen())

	quit := make(chan struct{})
	go connectLoop(*sockPath, mirror, p, quit)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "saturday-thinking: %v\n", err)
		close(quit)
		os.Exit(1)
	}
	close(quit)
}
