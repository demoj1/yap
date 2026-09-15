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
	rates    map[*peer]*rate // incoming kbps per peer, sampled once a second
	cursor   int             // selected tile in the roster
	width    int             // terminal columns, for the tile grid
	logs     []string
	frame    int
}

// rate turns a cumulative byte counter into kbps over ~1 s windows.
type rate struct {
	bytes uint64
	at    time.Time
	kbps  float64
}

func (r *rate) feed(bytes uint64, now time.Time) {
	if r.at.IsZero() {
		r.bytes, r.at = bytes, now
		return
	}
	if d := now.Sub(r.at); d >= time.Second {
		r.kbps = float64(bytes-r.bytes) * 8 / 1000 / d.Seconds()
		r.bytes, r.at = bytes, now
	}
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
	u.prog = tea.NewProgram(model{n: n, logPath: logPath, meters: map[*peer]*meter{}, rates: map[*peer]*rate{}}, tea.WithAltScreen(), tea.WithMouseCellMotion())
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
		now := time.Time(msg)
		for _, p := range peers {
			alive[p] = true
			mt := m.meters[p]
			if mt == nil {
				mt = &meter{}
				m.meters[p] = mt
			}
			mt.feed(p.level.take(), m.frame)
			r := m.rates[p]
			if r == nil {
				r = &rate{}
				m.rates[p] = r
			}
			r.feed(p.rxBytes.Load(), now)
		}
		for p := range m.meters {
			if !alive[p] {
				delete(m.meters, p)
				delete(m.rates, p)
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
		return m.act(msg.String())
	case tea.MouseMsg:
		return m.mouse(msg)
	}
	return m, nil
}

// act performs one keyboard action; mouse events are translated into these.
func (m model) act(key string) (tea.Model, tea.Cmd) {
	ctl := m.n.ctl
	peers := m.n.peerList()
	var sel *peer
	if m.cursor < len(peers) {
		sel = peers[m.cursor]
	}
	switch key {
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
	return m, nil
}

// mouse maps a click or wheel event onto the same actions as the keys:
// click a tile to pick that person (your own tile toggles mute), wheel over
// a tile for that person's volume (your own: bitrate), click a help label.
func (m model) mouse(e tea.MouseMsg) (tea.Model, tea.Cmd) {
	lay := m.layout()
	wheel := e.Button == tea.MouseButtonWheelUp || e.Button == tea.MouseButtonWheelDown
	if !wheel && e.Action != tea.MouseActionPress {
		return m, nil
	}
	if i, ok := lay.tileAt(e.X, e.Y); ok {
		switch {
		case wheel && i == 0:
			if e.Button == tea.MouseButtonWheelUp {
				return m.act("+")
			}
			return m.act("-")
		case wheel:
			m.cursor = i - 1
			if e.Button == tea.MouseButtonWheelUp {
				return m.act("right")
			}
			return m.act("left")
		case i == 0:
			return m.act("m")
		default:
			m.cursor = i - 1
		}
		return m, nil
	}
	if e.Y == lay.helpY && !wheel {
		for _, s := range lay.help {
			if e.X >= s.x0 && e.X < s.x1 {
				key := s.key
				if e.Button == tea.MouseButtonRight && s.alt != "" {
					key = s.alt
				}
				return m.act(key)
			}
		}
	}
	return m, nil
}

// layout is the geometry View draws and mouse() hit-tests: tiles in a grid
// starting at row tileY, help labels on row helpY.
type layout struct {
	w, cols, tiles int
	tileY, helpY   int
	help           []helpSeg
}

type helpSeg struct {
	x0, x1   int
	label    string
	key, alt string // alt is the right-click action
}

const (
	tileY = 3 // blank, link, blank
	tileH = 5 // border, 3 lines, border
)

func (m model) layout() layout {
	peers := m.n.peerList()
	names := []string{m.n.name + " (you)"}
	for _, p := range peers {
		names = append(names, p.name)
	}
	w := tileWidth(names, m.width)
	lay := layout{w: w, tiles: len(names), tileY: tileY}
	if m.n.update.Load() != nil {
		lay.tileY++ // the update line sits under the link
	}
	lay.cols = max(1, (max(m.width, w+4)-2)/(w+2))
	rows := (lay.tiles + lay.cols - 1) / lay.cols
	y := lay.tileY + rows*tileH
	if len(peers) == 0 {
		y += 2
	}
	if m.n.set.Mic != "" || m.n.set.Out != "" {
		y++
	}
	lay.helpY = y + 1
	x := 2
	for _, s := range []helpSeg{
		{label: "↑/↓ pick", key: "down", alt: "up"},
		{label: "←/→ volume", key: "right", alt: "left"},
		{label: "m mute", key: "m"},
		{label: "d denoise", key: "d"},
		{label: "g gate", key: "g"},
		{label: "e echo", key: "e"},
		{label: "+/- bitrate", key: "+", alt: "-"},
		{label: "i mic", key: "i"},
		{label: "o out", key: "o"},
		{label: "q quit", key: "q"},
	} {
		s.x0, s.x1 = x, x+len([]rune(s.label))
		lay.help = append(lay.help, s)
		x = s.x1 + 2
	}
	return lay
}

// tileAt returns the tile index under a cell (0 = you), if any.
func (l layout) tileAt(x, y int) (int, bool) {
	if y < l.tileY || x < 2 {
		return 0, false
	}
	row, col := (y-l.tileY)/tileH, (x-2)/(l.w+2)
	if col >= l.cols {
		return 0, false
	}
	i := row*l.cols + col
	if i >= l.tiles {
		return 0, false
	}
	return i, true
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	tileMinW  = 30 // inner width of a roster tile; grows to fit the longest name
	tileExtra = 14 // room next to the name for "● 100 ms" or "MUTED"
)

var (
	tileSt    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	tileSelSt = tileSt.BorderForeground(lipgloss.Color("81"))
	tileMeSt  = tileSt.BorderForeground(lipgloss.Color("42"))
)

// tile renders one participant as a bordered card: name, status, meter, volume.
func tile(st lipgloss.Style, w int, name, status string, mt meter, foot string) string {
	body := fmt.Sprintf("%s %s\n%s\n%s", bold.Render(name), status, mt.bar(w-6), foot)
	return st.Width(w).Render(body)
}

// tileWidth fits every name on one line with its status, within the terminal.
func tileWidth(names []string, term int) int {
	w := tileMinW
	for _, n := range names {
		w = max(w, len([]rune(n))+tileExtra)
	}
	return min(w, max(tileMinW, term-6))
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
	fmt.Fprintf(&b, "\n  %s\n", linkSt.Render(m.n.link.String()))
	if u := m.n.update.Load(); u != nil {
		fmt.Fprintf(&b, "  %s\n", yellow.Render(*u))
	}
	b.WriteString("\n")

	mic := dim.Render("mic")
	if ctl.muted.Load() {
		mic = red.Render("MUTED")
	}
	dn := dim.Render("denoise off")
	if ctl.denoise.Load() {
		dn = green.Render("denoise on")
	}
	if ctl.gate.Load() {
		dn += " " + green.Render("gate")
	}
	if ctl.aec.Load() {
		dn += " " + green.Render("aec")
	}
	lay := m.layout()
	w := lay.w
	peers := m.n.peerList()
	me := tile(tileMeSt, w, m.n.name+" (you)", mic, m.mic,
		dim.Render(fmt.Sprintf("tx %d kbps", ctl.bitrate.Load()))+" "+dn)

	tiles := []string{me}
	for i, p := range peers {
		st := tileSt
		if i == m.cursor {
			st = tileSelSt
		}
		status := yellow.Render(spinner[m.frame%len(spinner)] + " punching")
		if p.connected() {
			path := "●"
			if p.via.Load() != nil {
				path = "◐" // through a relay
			}
			rtt := "…"
			if us := p.rttUS.Load(); us >= 1000 {
				rtt = fmt.Sprintf("%.0f ms", float64(us)/1000)
			} else if us > 0 {
				rtt = "<1 ms"
			}
			status = green.Render(path) + " " + dim.Render(rtt)
		}
		var mt meter
		if x := m.meters[p]; x != nil { // View can run before the tick that creates it
			mt = *x
		}
		vol := fmt.Sprintf("vol %3d%%", p.volume.Load())
		if i == m.cursor {
			vol = selSt.Render("◂ " + vol + " ▸")
		}
		kbps := ""
		if r := m.rates[p]; r != nil && r.kbps > 0 {
			kbps = dim.Render(fmt.Sprintf("  %.0f kbps", r.kbps))
		}
		tiles = append(tiles, tile(st, w, p.name, status, mt, vol+kbps))
	}

	for i := 0; i < len(tiles); i += lay.cols {
		row := tiles[i:min(i+lay.cols, len(tiles))]
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

	b.WriteString("  ")
	for i, s := range lay.help { // label = "<keys> <word>": keys highlighted, same cells mouse() hit-tests
		if i > 0 {
			b.WriteString("  ")
		}
		keys, word, _ := strings.Cut(s.label, " ")
		b.WriteString(keySt.Render(keys) + " " + word)
	}
	b.WriteString("\n")
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
