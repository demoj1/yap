package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	tick      = 33 * time.Millisecond
	meterLen  = 30
	logLines  = 6
	statsEach = time.Second
)

type (
	tickMsg    time.Time
	logMsg     string
	stateMsg   struct{ st, peer string }
	sessionMsg struct{ s *session }
)

var (
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	bold   = lipgloss.NewStyle().Bold(true)
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	linkSt = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	keySt  = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
)

// ui is the bubbletea front end; it implements view so the node can report
// into it from other goroutines.
type ui struct {
	prog *tea.Program
}

type model struct {
	n        *node
	st, peer string
	s        *session
	mic, spk meter
	stats    string
	statsAt  time.Time
	logs     []string
	frame    int
}

// meter is a VU bar with instant attack and slow release.
type meter struct {
	level, hold float64
	holdAt      int
}

func (m *meter) feed(db float64, frame int) {
	v := max(0, min(1, (db+50)/50))
	if v >= m.level {
		m.level = v
	} else {
		m.level *= 0.82
	}
	if v >= m.hold || frame-m.holdAt > 30 {
		m.hold, m.holdAt = v, frame
	}
}

func (m meter) String() string {
	var b strings.Builder
	lit := int(m.level*meterLen + 0.5)
	hold := int(m.hold*meterLen + 0.5)
	for i := 0; i < meterLen; i++ {
		st := dim
		switch {
		case i < lit && i >= meterLen*5/6:
			st = red
		case i < lit && i >= meterLen*2/3:
			st = yellow
		case i < lit:
			st = green
		}
		ch := "▮"
		if i >= lit {
			ch = "▯"
			if i == hold-1 && hold > lit {
				ch, st = "▮", dim
			}
		}
		b.WriteString(st.Render(ch))
	}
	return b.String()
}

func newUI(n *node) *ui {
	u := &ui{}
	u.prog = tea.NewProgram(model{n: n, statsAt: time.Now()}, tea.WithAltScreen())
	log.SetOutput(u)
	return u
}

func (u *ui) Run() error { _, err := u.prog.Run(); return err }

func (u *ui) Write(p []byte) (int, error) {
	u.prog.Send(logMsg(strings.TrimRight(string(p), "\n")))
	return len(p), nil
}

func (u *ui) state(st, peer string) { u.prog.Send(stateMsg{st, peer}) }
func (u *ui) session(s *session)    { u.prog.Send(sessionMsg{s}) }

func (m model) Init() tea.Cmd { return tea.Tick(tick, func(t time.Time) tea.Msg { return tickMsg(t) }) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.frame++
		m.mic.feed(m.n.audio.micPeak.take(), m.frame)
		m.spk.feed(m.n.audio.spkPeak.take(), m.frame)
		if m.s != nil && time.Since(m.statsAt) >= statsEach {
			m.stats = m.s.stats(m.n.audio, time.Since(m.statsAt))
			m.statsAt = time.Now()
		}
		return m, tea.Tick(tick, func(t time.Time) tea.Msg { return tickMsg(t) })
	case logMsg:
		m.logs = append(m.logs, string(msg))
		if len(m.logs) > logLines {
			m.logs = m.logs[len(m.logs)-logLines:]
		}
	case stateMsg:
		m.st, m.peer = msg.st, msg.peer
	case sessionMsg:
		m.s, m.stats = msg.s, ""
	case tea.KeyMsg:
		ctl := m.n.ctl
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			ctl.volume.Store(min(200, ctl.volume.Load()+10))
		case "down", "j":
			ctl.volume.Store(max(0, ctl.volume.Load()-10))
		case "m":
			ctl.muted.Store(!ctl.muted.Load())
		case "d":
			ctl.denoise.Store(!ctl.denoise.Load())
		case "+", "=":
			ctl.stepBitrate(+1)
		case "-", "_":
			ctl.stepBitrate(-1)
		}
	}
	return m, nil
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m model) View() string {
	ctl := m.n.ctl
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %s\n\n", linkSt.Render(m.n.link.String()))

	mic := "mic"
	if ctl.muted.Load() {
		mic = red.Render("MUTED")
	}
	dn := dim.Render("denoise off")
	if ctl.denoise.Load() {
		dn = green.Render("denoise on")
	}
	fmt.Fprintf(&b, "  %s %s %s  %-6s %4d kbps · %s\n",
		green.Render("●"), bold.Render(pad(m.n.name)), m.mic, mic, ctl.bitrate.Load(), dn)

	dot, peer, note := dim.Render("○"), dim.Render(pad("—")), ""
	switch m.st {
	case "waiting":
		note = dim.Render(spinner[m.frame%len(spinner)] + " waiting for a friend")
	case "calling":
		note = dim.Render(spinner[m.frame%len(spinner)] + " calling")
	case "punching":
		dot, peer = yellow.Render("●"), bold.Render(pad(m.peer))
		note = yellow.Render(spinner[m.frame%len(spinner)] + " punching NAT")
	case "connected":
		dot, peer = green.Render("●"), bold.Render(pad(m.peer))
		if m.s != nil {
			note = green.Render("connected ") + dim.Render(m.s.peer.Load().String())
		}
	}
	fmt.Fprintf(&b, "  %s %s %s  vol %3d%%  %s\n\n", dot, peer, m.spk, ctl.volume.Load(), note)

	if m.stats != "" {
		fmt.Fprintf(&b, "  %s\n\n", dim.Render(m.stats))
	}
	fmt.Fprintf(&b, "  %s volume  %s mute  %s denoise  %s bitrate  %s quit\n\n",
		keySt.Render("↑/↓"), keySt.Render("m"), keySt.Render("d"), keySt.Render("+/-"), keySt.Render("q"))
	for _, l := range m.logs {
		fmt.Fprintf(&b, "  %s\n", dim.Render(l))
	}
	return b.String()
}

// pad fixes the name column width before styling, since ANSI codes would
// otherwise count toward %-12s.
func pad(s string) string {
	if len([]rune(s)) > 12 {
		s = string([]rune(s)[:11]) + "…"
	}
	return fmt.Sprintf("%-12s", s)
}
