package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gen2brain/malgo"
)

const (
	tick     = 33 * time.Millisecond
	meterLen = 30
	logLines = 6
)

type (
	tickMsg   time.Time
	logMsg    string
	noticeMsg string
)

var (
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	bold   = lipgloss.NewStyle().Bold(true)
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	linkSt = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	keySt  = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	selSt  = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
)

// ui is the bubbletea front end. The node does not push state into it: on
// every tick the model reads the roster and levels straight from the node.
type ui struct {
	prog *tea.Program
}

type model struct {
	n        *node
	logPath  string
	notice   string
	noticeAt int
	mic      meter
	meters   map[*peer]*meter
	cursor   int // selected tile in the roster
	width    int // terminal columns, for the tile grid
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

func (m meter) String() string { return m.bar(meterLen) }

func (m meter) bar(n int) string {
	var b strings.Builder
	lit := int(m.level*float64(n) + 0.5)
	hold := int(m.hold*float64(n) + 0.5)
	for i := 0; i < n; i++ {
		st := dim
		switch {
		case i < lit && i >= n*5/6:
			st = red
		case i < lit && i >= n*2/3:
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

func newUI(n *node, logPath string) *ui {
	u := &ui{}
	u.prog = tea.NewProgram(model{n: n, logPath: logPath, meters: map[*peer]*meter{}}, tea.WithAltScreen())
	return u
}

func (u *ui) Run() error { _, err := u.prog.Run(); return err }

func (u *ui) Write(p []byte) (int, error) {
	u.prog.Send(logMsg(strings.TrimRight(string(p), "\n")))
	return len(p), nil
}

func (m model) Init() tea.Cmd { return tea.Tick(tick, func(t time.Time) tea.Msg { return tickMsg(t) }) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.frame++
		m.mic.feed(m.n.audio.micPeak.take(), m.frame)
		peers := m.n.peerList()
		alive := map[*peer]bool{}
		for _, p := range peers {
			alive[p] = true
			mt := m.meters[p]
			if mt == nil {
				mt = &meter{}
				m.meters[p] = mt
			}
			mt.feed(p.level.take(), m.frame)
		}
		for p := range m.meters {
			if !alive[p] {
				delete(m.meters, p)
			}
		}
		if m.cursor >= len(peers) {
			m.cursor = max(0, len(peers)-1)
		}
		return m, tea.Tick(tick, func(t time.Time) tea.Msg { return tickMsg(t) })
	case logMsg:
		m.logs = append(m.logs, string(msg))
		if len(m.logs) > logLines {
			m.logs = m.logs[len(m.logs)-logLines:]
		}
	case noticeMsg:
		m.notice, m.noticeAt = string(msg), m.frame
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		ctl := m.n.ctl
		peers := m.n.peerList()
		var sel *peer
		if m.cursor < len(peers) {
			sel = peers[m.cursor]
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			m.cursor = max(0, m.cursor-1)
		case "down", "j":
			m.cursor = min(max(0, len(peers)-1), m.cursor+1)
		case "right", "l":
			if sel != nil {
				m.n.setVolume(sel, int(min(200, sel.volume.Load()+10)))
			}
		case "left", "h":
			if sel != nil {
				m.n.setVolume(sel, int(max(0, sel.volume.Load()-10)))
			}
		case "m":
			ctl.muted.Store(!ctl.muted.Load())
		case "d":
			ctl.denoise.Store(!ctl.denoise.Load())
			m.n.set.Denoise = ctl.denoise.Load()
			m.n.set.save()
		case "+", "=":
			ctl.stepBitrate(+1)
			m.n.set.Bitrate = int(ctl.bitrate.Load())
			m.n.set.save()
		case "-", "_":
			ctl.stepBitrate(-1)
			m.n.set.Bitrate = int(ctl.bitrate.Load())
			m.n.set.save()
		case "i":
			return m, func() tea.Msg { return noticeMsg(m.n.cycleDevice(malgo.Capture)) }
		case "o":
			return m, func() tea.Msg { return noticeMsg(m.n.cycleDevice(malgo.Playback)) }
		}
	}
	return m, nil
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	tileW     = 24 // inner width of a roster tile
	tileMeter = 20
)

var (
	tileSt    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1).Width(tileW)
	tileSelSt = tileSt.BorderForeground(lipgloss.Color("81"))
	tileMeSt  = tileSt.BorderForeground(lipgloss.Color("42"))
)

// tile renders one participant as a bordered card: name, status, meter, volume.
func tile(st lipgloss.Style, name, status string, mt meter, foot string) string {
	body := fmt.Sprintf("%s %s\n%s\n%s", bold.Render(trunc(name, tileW-4)), status, mt.bar(tileMeter), foot)
	return st.Render(body)
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (m model) View() string {
	ctl := m.n.ctl
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %s\n\n", linkSt.Render(m.n.link.String()))

	mic := dim.Render("mic")
	if ctl.muted.Load() {
		mic = red.Render("MUTED")
	}
	dn := dim.Render("denoise off")
	if ctl.denoise.Load() {
		dn = green.Render("denoise on")
	}
	me := tile(tileMeSt, m.n.name+" (you)", mic, m.mic,
		dim.Render(fmt.Sprintf("tx %d kbps", ctl.bitrate.Load()))+" "+dn)

	peers := m.n.peerList()
	tiles := []string{me}
	for i, p := range peers {
		st := tileSt
		if i == m.cursor {
			st = tileSelSt
		}
		status := yellow.Render(spinner[m.frame%len(spinner)] + " punching")
		if p.connected() {
			status = green.Render("●") + " " + dim.Render(fmt.Sprintf("%.0f ms", float64(p.jitUS.Load())/1000))
		}
		var mt meter
		if x := m.meters[p]; x != nil { // View can run before the tick that creates it
			mt = *x
		}
		vol := fmt.Sprintf("vol %3d%%", p.volume.Load())
		if i == m.cursor {
			vol = selSt.Render("◂ " + vol + " ▸")
		}
		tiles = append(tiles, tile(st, p.name, status, mt, vol))
	}

	cols := max(1, (max(m.width, tileW+4)-2)/(tileW+3))
	for i := 0; i < len(tiles); i += cols {
		row := tiles[i:min(i+cols, len(tiles))]
		b.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(lipgloss.JoinHorizontal(lipgloss.Top, row...)))
		b.WriteString("\n")
	}
	if len(peers) == 0 {
		fmt.Fprintf(&b, "\n  %s\n", dim.Render(spinner[m.frame%len(spinner)]+" waiting for friends — send them the link"))
	}
	if m.n.set.Mic != "" || m.n.set.Out != "" {
		fmt.Fprintf(&b, "  %s\n", dim.Render(fmt.Sprintf("mic %s · out %s", orDefault(m.n.set.Mic), orDefault(m.n.set.Out))))
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "  %s pick  %s volume  %s mute  %s denoise  %s bitrate  %s mic  %s out  %s quit\n",
		keySt.Render("↑/↓"), keySt.Render("←/→"), keySt.Render("m"), keySt.Render("d"), keySt.Render("+/-"),
		keySt.Render("i"), keySt.Render("o"), keySt.Render("q"))
	if m.notice != "" && m.frame-m.noticeAt < 90 {
		fmt.Fprintf(&b, "  %s\n", yellow.Render(m.notice))
	} else {
		b.WriteString("\n")
	}
	for _, l := range m.logs {
		fmt.Fprintf(&b, "  %s\n", dim.Render(l))
	}
	fmt.Fprintf(&b, "\n  %s\n", dim.Render("full log: "+m.logPath))
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

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
