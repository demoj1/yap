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
	logKeep  = 200 // log lines remembered; render shows as many as fit below the controls
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
	hotSt  = lipgloss.NewStyle().Bold(true)
)

const devRefresh = 150 // frames (~5 s) between device list refreshes

// hot renders word with its hotkey letter highlighted in place, so the key
// is read off the label itself: "mic" with a lit m, "gain" with a lit a.
func hot(word, key string) string {
	if i := strings.Index(word, key); i >= 0 {
		return word[:i] + hotSt.Render(key) + word[i+len(key):]
	}
	return hotSt.Render(key) + " " + word
}

// people are the peers that get a tile: everyone but relays.
func (m model) people() []*peer {
	var out []*peer
	for _, p := range m.n.peerList() {
		if !p.relay {
			out = append(out, p)
		}
	}
	return out
}

func (m model) relays() []*peer {
	var out []*peer
	for _, p := range m.n.peerList() {
		if p.relay {
			out = append(out, p)
		}
	}
	return out
}

// rttText is the round trip as shown next to a peer, "…" until the first pong.
func rttText(p *peer) string {
	if us := p.rttUS.Load(); us >= 1000 {
		return fmt.Sprintf("%.0f ms", float64(us)/1000)
	} else if us > 0 {
		return "<1 ms"
	}
	return "…"
}

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
	height   int             // terminal rows: the log fills whatever the controls leave
	inputs   []string        // device lists shown as tiles; refreshed every devRefresh frames
	outputs  []string
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
		if m.frame%devRefresh == 1 {
			m.inputs, _ = m.n.deviceNames(malgo.Capture)
			m.outputs, _ = m.n.deviceNames(malgo.Playback)
		}
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
		if np := len(m.people()); m.cursor >= np {
			m.cursor = max(0, np-1)
		}
		return m, tea.Tick(tick, func(t time.Time) tea.Msg { return tickMsg(t) })
	case logMsg:
		m.logs = append(m.logs, string(msg))
		if len(m.logs) > logKeep {
			m.logs = m.logs[len(m.logs)-logKeep:]
		}
	case noticeMsg:
		m.notice, m.noticeAt = string(msg), m.frame
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		return m.act(msg.String())
	case tea.MouseMsg:
		return m.mouse(msg)
	}
	return m, nil
}

const volStep = 3 // percent per volume nudge

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// note flashes a line so every action has visible feedback.
func (m model) note(s string) model { m.notice, m.noticeAt = s, m.frame; return m }

// step nudges the selected peer's volume, or the bitrate, up or down.
func (m model) step(what string, up bool) (tea.Model, tea.Cmd) {
	ctl := m.n.ctl
	if what == "bitrate" {
		if up {
			ctl.stepBitrate(+1)
		} else {
			ctl.stepBitrate(-1)
		}
		m.n.set.Bitrate = int(ctl.bitrate.Load())
		m.n.set.save()
		return m.note(fmt.Sprintf("your bitrate %d kbps", ctl.bitrate.Load())), nil
	}
	peers := m.people()
	if m.cursor >= len(peers) {
		return m, nil
	}
	p := peers[m.cursor]
	v := int(p.volume.Load())
	if up {
		v = min(200, v+volStep)
	} else {
		v = max(0, v-volStep)
	}
	m.n.setVolume(p, v)
	return m.note(fmt.Sprintf("%s volume %d%%", p.name, v)), nil
}

// act performs one keyboard action; mouse events are translated into these.
func (m model) act(key string) (tea.Model, tea.Cmd) {
	ctl := m.n.ctl
	peers := m.people()
	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.cursor = max(0, m.cursor-1)
	case "down", "j":
		m.cursor = min(max(0, len(peers)-1), m.cursor+1)
	case "right":
		return m.step("volume", true)
	case "left":
		return m.step("volume", false)
	case "+", "=":
		return m.step("bitrate", true)
	case "-", "_":
		return m.step("bitrate", false)
	case "m":
		ctl.muted.Store(!ctl.muted.Load())
		return m.note("mic " + map[bool]string{true: "MUTED", false: "on"}[ctl.muted.Load()]), nil
	case "d":
		ctl.denoise.Store(!ctl.denoise.Load())
		m.n.set.Denoise = ctl.denoise.Load()
		m.n.set.save()
		return m.note("denoise " + onOff(ctl.denoise.Load())), nil
	case "g":
		ctl.gate.Store(!ctl.gate.Load())
		m.n.set.Gate = ctl.gate.Load()
		m.n.set.save()
		return m.note("noise gate " + onOff(ctl.gate.Load())), nil
	case "e":
		ctl.aec.Store(!ctl.aec.Load())
		m.n.audio.aecOn.Store(ctl.aec.Load())
		m.n.set.AEC = ctl.aec.Load()
		m.n.set.save()
		return m.note("echo cancel " + onOff(ctl.aec.Load())), nil
	case "a":
		ctl.agc.Store(!ctl.agc.Load())
		m.n.set.AGC = ctl.agc.Load()
		m.n.set.save()
		return m.note("auto-gain " + onOff(ctl.agc.Load())), nil
	case "l":
		return m.note(m.n.toggleLock()), nil
	case "i":
		return m.nextDevice(malgo.Capture)
	case "o":
		return m.nextDevice(malgo.Playback)
	}
	return m, nil
}

// nextDevice cycles the microphone or speaker to the next one in its tile.
func (m model) nextDevice(kind malgo.DeviceType) (tea.Model, tea.Cmd) {
	names, cur := m.n.deviceNames(kind)
	return m.choose(kind, names[(cur+1)%len(names)])
}

// choose switches to a device and refreshes the tiles so the mark moves now.
func (m model) choose(kind malgo.DeviceType, name string) (tea.Model, tea.Cmd) {
	m = m.note(m.n.useDevice(kind, name))
	m.inputs, _ = m.n.deviceNames(malgo.Capture)
	m.outputs, _ = m.n.deviceNames(malgo.Playback)
	return m, nil
}

// seg is a clickable text span on a known row: cells [x0,x1) trigger key
// (or alt on right-click).
type seg struct {
	x0, x1   int
	key, alt string
}

// geometry is where render() put things, so mouse() can hit-test exactly the
// same layout that was drawn.
type geometry struct {
	tileTop, tileH, stride, cols, tiles int
	togglesRow, actionsRow              int
	toggles, actions                    []seg
	devTop, devStride                   int // device tiles: rows start 2 below the top (border + title)
	devRows                             [2]int
}

const (
	tileH   = 5 // border, 3 lines, border
	leftPad = 2
)

// render builds the whole screen as lines plus the geometry of everything
// clickable. View joins the lines; mouse() reads the geometry — they can
// never disagree because both come from here.
func (m model) render() ([]string, geometry) {
	ctl := m.n.ctl
	var g geometry
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	add("")
	add("  " + linkSt.Render(m.n.link.String()))
	if u := m.n.update.Load(); u != nil {
		add("  " + yellow.Render(*u))
	}
	add("")

	peers := m.people()
	names := []string{m.n.name + " (you)"}
	for _, p := range peers {
		names = append(names, p.name)
	}
	w := tileWidth(names, m.width)

	tiles := []string{m.selfTile(w)}
	for i, p := range peers {
		tiles = append(tiles, m.peerTile(p, i, w))
	}
	g.stride = max(1, lipgloss.Width(strings.SplitN(tiles[0], "\n", 2)[0]))
	g.cols = max(1, (max(m.width, g.stride+leftPad)-leftPad)/g.stride)
	g.tileH, g.tiles, g.tileTop = tileH, len(tiles), len(lines)
	for i := 0; i < len(tiles); i += g.cols {
		row := tiles[i:min(i+g.cols, len(tiles))]
		block := lipgloss.NewStyle().PaddingLeft(leftPad).Render(lipgloss.JoinHorizontal(lipgloss.Top, row...))
		for _, ln := range strings.Split(block, "\n") {
			add(ln)
		}
	}
	if len(peers) == 0 {
		add("")
		add("  " + dim.Render(spinner[m.frame%len(spinner)]+" waiting for friends — send them the link"))
	}
	// Relays are plumbing, not people: one line each, no tile, no volume.
	for _, r := range m.relays() {
		state := yellow.Render(spinner[m.frame%len(spinner)] + " connecting")
		if r.connected() {
			via := ""
			if a := r.addr.Load(); a != nil {
				via = a.String() + " · "
			}
			state = dim.Render(via + rttText(r))
		}
		add("  " + keySt.Render("⇄ "+r.name) + " " + state)
	}
	add("")

	// Toggles: always visible with explicit ON/off, so a keypress visibly flips one.
	g.togglesRow = len(lines)
	line, segs, x := "", []seg(nil), leftPad
	chip := func(key, name string, on, offRed bool) {
		sw, st := "○ off", dim
		if on {
			sw, st = "● on", green
		} else if offRed {
			st = red
		}
		plain := name + " " + sw
		segs = append(segs, seg{x, x + len([]rune(plain)), key, ""})
		line += hot(name, key) + " " + st.Render(sw) + "    "
		x += len([]rune(plain)) + 4
	}
	chip("m", "mic", !ctl.muted.Load(), true)
	chip("d", "denoise", ctl.denoise.Load(), false)
	chip("g", "gate", ctl.gate.Load(), false)
	chip("e", "echo", ctl.aec.Load(), false)
	chip("a", "gain", ctl.agc.Load(), false)
	chip("l", "lock", m.n.locked.Load(), false)
	g.toggles = segs
	add("  " + line)

	// Actions: labels that do something on click; keys shown highlighted.
	g.actionsRow = len(lines)
	line, segs, x = "", nil, leftPad
	// Arrow/sign actions show their keys in front; letter actions light the
	// letter inside the word, like the toggles above.
	action := func(keys, word, key, alt string) {
		plain, shown := word, hot(word, key)
		if keys != "" {
			plain, shown = keys+" "+word, keySt.Render(keys)+" "+word
		}
		segs = append(segs, seg{x, x + len([]rune(plain)), key, alt})
		line += shown + "   "
		x += len([]rune(plain)) + 3
	}
	action("↑/↓", "pick", "down", "up")
	action("←/→", "volume", "right", "left")
	action("+/-", "bitrate", "+", "-")
	action("", "input", "i", "")
	action("", "output", "o", "")
	action("", "quit", "q", "")
	g.actions = segs
	add("  " + line)
	add("")

	// Device tiles: every microphone and speaker listed, the one in use
	// marked; a click on a row switches to it.
	dw := max(tileMinW, min(m.width/2-leftPad-1, 60))
	in := deviceTile(dw, "input", "i", m.inputs, m.n.audio.mic)
	out := deviceTile(dw, "output", "o", m.outputs, m.n.audio.out)
	g.devTop, g.devStride = len(lines), lipgloss.Width(strings.SplitN(in, "\n", 2)[0])
	g.devRows = [2]int{len(m.inputs), len(m.outputs)}
	block := lipgloss.NewStyle().PaddingLeft(leftPad).Render(lipgloss.JoinHorizontal(lipgloss.Top, in, out))
	for _, ln := range strings.Split(block, "\n") {
		add(ln)
	}

	if m.notice != "" && m.frame-m.noticeAt < 60 {
		add("  " + yellow.Render("▸ "+m.notice))
	} else {
		add("")
	}
	// The log takes every row left below the controls, newest at the bottom,
	// with the file path as the last line.
	show := 3
	if m.height > 0 {
		show = max(0, m.height-len(lines)-1)
	}
	logs := m.logs
	if len(logs) > show {
		logs = logs[len(logs)-show:]
	}
	for _, l := range logs {
		if m.width > leftPad+8 {
			l = trunc(l, m.width-leftPad)
		}
		add("  " + dim.Render(l))
	}
	add("  " + dim.Render("full log: "+m.logPath))
	return lines, g
}

func (m model) selfTile(w int) string {
	ctl := m.n.ctl
	head := "" // the name already says "(you)"; only a mute needs shouting
	if ctl.muted.Load() {
		head = red.Render("● MUTED")
	}
	return tile(tileMeSt, w, m.n.name+" (you)", head,
		m.mic, dim.Render(fmt.Sprintf("tx %d kbps", ctl.bitrate.Load())))
}

func (m model) peerTile(p *peer, i, w int) string {
	st := tileSt
	if i == m.cursor {
		st = tileSelSt
	}
	status := yellow.Render(spinner[m.frame%len(spinner)] + " connecting")
	if p.connected() {
		path := "●"
		if p.via.Load() != nil {
			path = "◐"
		}
		status = green.Render(path) + " " + dim.Render(rttText(p))
	}
	var mt meter
	if x := m.meters[p]; x != nil {
		mt = *x
	}
	vol := fmt.Sprintf("vol %3d%%", p.volume.Load())
	if i == m.cursor {
		vol = selSt.Render("◂ " + vol + " ▸")
	} else {
		vol = dim.Render(vol)
	}
	if r := m.rates[p]; r != nil && r.kbps > 0 {
		vol += dim.Render(fmt.Sprintf("  %.0f kbps", r.kbps))
	}
	return tile(st, w, p.name, status, mt, vol)
}

// mouse maps clicks and wheel onto actions using the exact drawn geometry.
func (m model) mouse(e tea.MouseMsg) (tea.Model, tea.Cmd) {
	_, g := m.render()
	wheel := e.Button == tea.MouseButtonWheelUp || e.Button == tea.MouseButtonWheelDown
	up := e.Button == tea.MouseButtonWheelUp
	if !wheel && e.Action != tea.MouseActionPress {
		return m, nil
	}
	if !wheel && e.X >= leftPad && e.Y >= g.devTop+2 { // a device row?
		col, row := (e.X-leftPad)/max(1, g.devStride), e.Y-g.devTop-2
		if col < 2 && row < g.devRows[col] {
			if col == 0 {
				return m.choose(malgo.Capture, m.inputs[row])
			}
			return m.choose(malgo.Playback, m.outputs[row])
		}
	}
	if i, ok := g.tileAt(e.X, e.Y); ok {
		switch {
		case wheel && i == 0:
			return m.step("bitrate", up)
		case wheel:
			m.cursor = i - 1
			return m.step("volume", up)
		case i == 0:
			return m.act("m")
		default:
			m.cursor = i - 1
			return m, nil
		}
	}
	hit := func(row int, segs []seg) (tea.Model, tea.Cmd, bool) {
		if e.Y != row || wheel {
			return m, nil, false
		}
		for _, s := range segs {
			if e.X >= s.x0 && e.X < s.x1 {
				key := s.key
				if e.Button == tea.MouseButtonRight && s.alt != "" {
					key = s.alt
				}
				mm, cmd := m.act(key)
				return mm, cmd, true
			}
		}
		return m, nil, false
	}
	if mm, cmd, ok := hit(g.togglesRow, g.toggles); ok {
		return mm, cmd
	}
	if mm, cmd, ok := hit(g.actionsRow, g.actions); ok {
		return mm, cmd
	}
	return m, nil
}

// tileAt returns the tile index under a cell (0 = you), if any.
func (g geometry) tileAt(x, y int) (int, bool) {
	if y < g.tileTop || x < leftPad {
		return 0, false
	}
	row, col := (y-g.tileTop)/g.tileH, (x-leftPad)/g.stride
	if col >= g.cols {
		return 0, false
	}
	if i := row*g.cols + col; i < g.tiles {
		return i, true
	}
	return 0, false
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	tileMinW  = 30
	tileExtra = 14
)

var (
	tileSt    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	tileSelSt = tileSt.BorderForeground(lipgloss.Color("81"))
	tileMeSt  = tileSt.BorderForeground(lipgloss.Color("42"))
)

func tile(st lipgloss.Style, w int, name, status string, mt meter, foot string) string {
	head := bold.Render(trunc(name, w-4))
	if status != "" {
		head += " " + status
	}
	return st.Width(w).Render(fmt.Sprintf("%s\n%s\n%s", head, mt.bar(w-6), foot))
}

// deviceTile lists devices under a hotkey-lit title, marking the one in use.
func deviceTile(w int, title, key string, names []string, using string) string {
	rows := []string{hot(title, key)}
	for _, name := range names {
		if name == using {
			rows = append(rows, green.Render("● "+trunc(orDefault(name), w-6)))
		} else {
			rows = append(rows, dim.Render("  "+trunc(orDefault(name), w-6)))
		}
	}
	return tileSt.Width(w).Render(strings.Join(rows, "\n"))
}

func tileWidth(names []string, term int) int {
	w := tileMinW
	for _, n := range names {
		w = max(w, len([]rune(n))+tileExtra)
	}
	if term > 0 {
		w = min(w, max(tileMinW, term-6))
	}
	return w
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (m model) View() string {
	lines, _ := m.render()
	return strings.Join(lines, "\n")
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
