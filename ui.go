package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gen2brain/malgo"
)

const (
	tick     = 50 * time.Millisecond // 20 fps: meters stay fluid, a frame is the main idle cost
	meterLen = 30
	logKeep  = 200 // log lines remembered; render shows as many as fit below the controls
)

type (
	tickMsg   time.Time
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

const noReplyAfter = 3 * time.Second // a connected peer silent this long is flagged on its tile

// copyToClipboard puts s on the clipboard every way that might work: OSC 52
// through the terminal (wrapped for tmux, so it survives ssh), then the
// first local clipboard tool found.
func copyToClipboard(s string) {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\x07"
	if os.Getenv("TMUX") != "" {
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	os.Stdout.WriteString(seq)
	for _, tool := range [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}, {"pbcopy"}, {"clip"}} {
		if _, err := exec.LookPath(tool[0]); err != nil {
			continue
		}
		cmd := exec.Command(tool[0], tool[1:]...)
		cmd.Stdin = strings.NewReader(s)
		if cmd.Run() == nil {
			return
		}
	}
}

// turn nudges the selected knob by dir steps.
func (m model) turn(dir int) (tea.Model, tea.Cmd) {
	return m.note(m.n.turnKnob(m.tune, dir)), nil
}

// hot renders word with its hotkey letter highlighted in place, so the key
// is read off the label itself: "mic" with a lit m, "gain" with a lit a.
func hot(word, key string) string {
	i := strings.Index(word, key) // every hotkey is a letter of its word
	return word[:i] + hotSt.Render(key) + word[i+len(key):]
}

// verText is a peer's announced build; anything older than that says so.
func verText(p *peer) string {
	if p.ver == "" {
		return "<0.8"
	}
	return p.ver
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
// every tick the model reads the roster, levels and chat straight from it.
type ui struct {
	prog *tea.Program
}

type model struct {
	n        *node
	logPath  string
	notice   string
	noticeAt int
	mic      meter
	views    map[*peer]*view // per-peer screen state: meter, rates, whether they chimed in
	cursor   int             // selected tile in the roster
	width    int             // terminal columns, for the tile grid
	height   int             // terminal rows: the log fills whatever the controls leave
	tune     int             // selected row of the tuning tile
	cache    *panelCache     // device + tuning tiles, rebuilt only when they change
	asked    bool            // the update dialog was answered (either way)
	doUpdate bool            // the answer was yes: main updates and restarts after the TUI exits
	typing   bool            // keys go to the chat line, not the switches
	input    string          // the chat line being typed
	frame    int
}

// view is what the screen keeps about one peer between frames.
type view struct {
	meter meter
	rate  rate
	seen  bool // heard from at least once: they chimed in, and will chime out
	tile  tileCache
}

// tileCache keeps a rendered tile until anything visible on it changes:
// laying out a bordered box is the costliest thing a frame does, and most
// tiles sit still most of the time.
type tileCache struct{ key, out string }

func (c *tileCache) get(key string, render func() string) string {
	if c.key != key {
		c.key, c.out = key, render()
	}
	return c.out
}

// rate turns a peer's cumulative counters into what the status bar shows:
// incoming kbps over ~1 s windows, and "bad" — an average of recent drop
// events (lost, stall, skip, rebuffer) that decays over a few seconds, so
// quality reflects the last moments, not the whole call.
type rate struct {
	bytes uint64
	drops uint64
	at    time.Time
	kbps  float64
	bad   float64
}

func (r *rate) feed(bytes, drops uint64, now time.Time) {
	if r.at.IsZero() {
		r.bytes, r.drops, r.at = bytes, drops, now
		return
	}
	if d := now.Sub(r.at); d >= time.Second {
		r.kbps = float64(bytes-r.bytes) * 8 / 1000 / d.Seconds()
		r.bad = r.bad*0.7 + float64(drops-r.drops)
		r.bytes, r.drops, r.at = bytes, drops, now
	}
}

// drops is every event where the listener heard something other than the
// frame that was sent: a lost packet, a stall, a skipped frame, a rebuffer.
func drops(p *peer) uint64 {
	return p.jb.lost.Load() + p.jb.stall.Load() + p.jb.skip.Load() + p.jb.rebuf.Load()
}

// quality grades recent drop events for the status bar.
func quality(bad float64) string {
	switch {
	case bad < 0.5:
		return green.Render("good")
	case bad < 3:
		return yellow.Render("ok")
	default:
		return red.Render("poor")
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

// cells quantizes the meter to what an n-cell bar shows: lit cells and the
// peak-hold cell. Two meters with equal cells draw the same bar.
func (m meter) cells(n int) (lit, hold int) {
	return int(m.level*float64(n) + 0.5), int(m.hold*float64(n) + 0.5)
}

// band is the speech level band a tile reacts to: 0 silent, then · ∙ •.
func (m meter) band() int {
	switch {
	case m.level >= peakLvl:
		return 3
	case m.level >= loudLvl:
		return 2
	case m.level >= speakLvl:
		return 1
	}
	return 0
}

func (m meter) bar(n int) string {
	var b strings.Builder
	lit, hold := m.cells(n)
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

// newUI builds the screen; notice, if any, is shown for the first ~10 s.
func newUI(n *node, logPath, notice string) *ui {
	u := &ui{}
	m := model{n: n, logPath: logPath, views: map[*peer]*view{}, cache: &panelCache{},
		notice: notice, noticeAt: 200} // a notice lives 60 frames past noticeAt: this one for ~13 s
	u.prog = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	return u
}

// Run blocks until the user quits; true means they chose to update.
func (u *ui) Run() (bool, error) {
	final, err := u.prog.Run()
	if err != nil {
		return false, err
	}
	return final.(model).doUpdate, nil
}

func (m model) Init() tea.Cmd { return tea.Tick(tick, func(t time.Time) tea.Msg { return tickMsg(t) }) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.frame++
		m.mic.feed(math.Float64frombits(m.n.micDB.Load()), m.frame)
		alive := map[*peer]bool{}
		for _, p := range m.n.peerList() {
			alive[p] = true
			v := m.views[p]
			if v == nil {
				v = &view{}
				m.views[p] = v
			}
			v.meter.feed(math.Float64frombits(p.levelDB.Load()), m.frame)
			v.rate.feed(p.rxBytes.Load(), drops(p), time.Time(msg))
			if !v.seen && !p.relay && p.connected() { // a person arrived
				v.seen = true
				m.n.cue(cueJoin)
			}
		}
		for p, v := range m.views {
			if !alive[p] {
				if v.seen {
					m.n.cue(cueLeave)
				}
				delete(m.views, p)
			}
		}
		if np := len(m.n.people()); m.cursor >= np {
			m.cursor = max(0, np-1)
		}
		return m, tea.Tick(tick, func(t time.Time) tea.Msg { return tickMsg(t) })
	case noticeMsg:
		m.notice, m.noticeAt = string(msg), m.frame
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		if m.typing || msg.Paste { // pasting opens the chat line by itself
			m.typing = true
			return m.typeKey(msg)
		}
		return m.act(msg.String())
	case tea.MouseMsg:
		return m.mouse(msg)
	}
	return m, nil
}

// note flashes a line so every action has visible feedback.
func (m model) note(s string) model { m.notice, m.noticeAt = s, m.frame; return m }

// nudgeVolume moves the selected person's volume by dir steps.
func (m model) nudgeVolume(dir int) (tea.Model, tea.Cmd) {
	people := m.n.people()
	if m.cursor >= len(people) {
		return m, nil
	}
	return m.note(m.n.nudgeVolume(people[m.cursor], dir)), nil
}

func (m model) nudgeBitrate(dir int) (tea.Model, tea.Cmd) { return m.note(m.n.nudgeBitrate(dir)), nil }

// typeKey edits the chat line: enter sends, esc drops it, the rest types.
func (m model) typeKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEsc:
		m.typing, m.input = false, ""
	case tea.KeyEnter:
		if k.Alt { // alt+enter breaks the line: most terminals cannot tell shift+enter from enter
			m.input += "\n"
			break
		}
		m.n.say(m.input)
		m.typing, m.input = false, ""
	case tea.KeyCtrlJ:
		m.input += "\n"
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		m.input += " "
	case tea.KeyRunes: // typed or pasted (a paste keeps its line breaks)
		m.input += string(k.Runes)
	}
	return m, nil
}

// act performs one keyboard action; mouse events are translated into these.
func (m model) act(key string) (tea.Model, tea.Cmd) {
	if notice := m.n.press(key); notice != "" {
		return m.note(notice), nil
	}
	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "y", "n":
		if m.n.update.Load() == nil || m.asked {
			return m, nil
		}
		m.asked = true
		if key == "n" {
			return m.note("staying on " + version + " — run yap update whenever"), nil
		}
		m.doUpdate = true
		return m, tea.Quit
	case "up", "k":
		m.cursor = max(0, m.cursor-1)
	case "down", "j":
		m.cursor = min(max(0, len(m.n.people())-1), m.cursor+1)
	case "right":
		return m.nudgeVolume(+1)
	case "left":
		return m.nudgeVolume(-1)
	case "+", "=":
		return m.nudgeBitrate(+1)
	case "-", "_":
		return m.nudgeBitrate(-1)
	case " ":
		if ctl := m.n.ctl; ctl.ptt.Load() {
			wasTalking := ctl.talking()
			ctl.pressTalk()
			if !wasTalking {
				m.n.sendState()
			}
		}
	case "c":
		copyToClipboard(m.n.link.String())
		return m.note("link copied"), nil
	case "t", "enter":
		m.typing = true
	case "tab":
		m.tune = (m.tune + 1) % len(m.n.knobs())
	case "[":
		return m.turn(-1)
	case "]":
		return m.turn(+1)
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

// choose switches to a device.
func (m model) choose(kind malgo.DeviceType, name string) (tea.Model, tea.Cmd) {
	return m.note(m.n.useDevice(kind, name)), nil
}

// seg is a clickable text span on a known row: cells [x0,x1) trigger key
// (or alt on right-click).
type seg struct {
	x0, x1   int
	key, alt string
	row      int
}

// devBox is where one device tile's rows landed, for clicks.
type devBox struct {
	top, x0, x1, rows int // rows start 2 below top (border + title)
}

// geometry is where render() put things, so mouse() can hit-test exactly the
// same layout that was drawn.
type geometry struct {
	tileTop, tileH, stride, cols, tiles int
	linkRow                             int   // a click on the link copies it
	ctl                                 []seg // toggles, actions and the update prompt, each on its row
	dev                                 [2]devBox
	tune                                devBox   // the tuning tile: wheel turns the row under the pointer
	arrows                              [][2]int // per knob row: x of its ◂ and ▸
}

const (
	tileH   = 5 // border, 3 lines, border
	leftPad = 2
)

// render builds the whole screen as lines plus the geometry of everything
// clickable. View joins the lines; mouse() reads the geometry — they can
// never disagree because both come from here.
func (m model) render() ([]string, geometry) {
	s := &screen{model: m}
	s.header()
	s.tiles()
	s.controls()
	s.panels()
	s.bottom()
	return s.lines, s.g
}

// screen accumulates the frame top to bottom.
type screen struct {
	model
	lines []string
	g     geometry
}

func (s *screen) add(line string) { s.lines = append(s.lines, line) }

// clipped adds a free-text line cut to the terminal width: one that wrapped
// would shift every row below it and break the click geometry.
func (s *screen) clipped(line string) {
	if s.width > 0 {
		line = lipgloss.NewStyle().MaxWidth(s.width).Render(line)
	}
	s.add(line)
}

func (s *screen) header() {
	s.add("")
	s.g.linkRow = len(s.lines)
	s.clipped("  " + linkSt.Render(s.n.link.String()) + dim.Render("   c to copy"))
	if tag := s.n.update.Load(); tag != nil {
		if s.asked {
			s.clipped("  " + dim.Render(*tag+" is out — yap update"))
		} else { // the dialog: y updates and restarts into the same room, n dismisses
			lead := fmt.Sprintf("⬆ %s available (you run %s) — update now?   ", *tag, version)
			yes, no := "[y] yes", "[n] later"
			x, row := leftPad+len([]rune(lead)), len(s.lines)
			s.g.ctl = append(s.g.ctl, seg{x, x + len(yes), "y", "", row}, seg{x + len(yes) + 3, x + len(yes) + 3 + len(no), "n", "", row})
			s.clipped("  " + yellow.Render(lead) + hotSt.Render(yes) + "   " + hotSt.Render(no))
		}
	}
	s.clipped("  " + s.statusBar())
	s.add("")
}

// tiles is the grid of people (us first), then one line per relay.
func (s *screen) tiles() {
	people := s.n.people()
	names := []string{s.n.name + " (you)"}
	for _, p := range people {
		names = append(names, p.name)
	}
	w := tileWidth(names, s.width)
	tiles := []string{s.selfTile(w)}
	for i, p := range people {
		tiles = append(tiles, s.peerTile(p, i, w))
	}
	g := &s.g
	g.stride = max(1, lipgloss.Width(strings.SplitN(tiles[0], "\n", 2)[0]))
	g.cols = max(1, (max(s.width, g.stride+leftPad)-leftPad)/g.stride)
	g.tileH, g.tiles, g.tileTop = tileH, len(tiles), len(s.lines)
	for i := 0; i < len(tiles); i += g.cols {
		row := lipgloss.JoinHorizontal(lipgloss.Top, tiles[i:min(i+g.cols, len(tiles))]...)
		s.lines = append(s.lines, strings.Split(lipgloss.NewStyle().PaddingLeft(leftPad).Render(row), "\n")...)
	}
	if len(people) == 0 {
		s.add("")
		s.add("  " + dim.Render(spinner[s.frame%len(spinner)]+" waiting for friends — send them the link"))
	}
	for _, r := range s.n.peerList() { // relays are plumbing, not people: a line, no tile, no volume
		if !r.relay {
			continue
		}
		state := yellow.Render(spinner[s.frame%len(spinner)] + " connecting")
		if r.connected() {
			via := ""
			if a := r.addr.Load(); a != nil {
				via = a.String() + " · "
			}
			state = dim.Render(via + rttText(r))
		}
		s.clipped("  " + keySt.Render("⇄ "+r.name) + " " + dim.Render(verText(r)) + " " + state)
	}
	s.add("")
}

// controls is the toggle row and the action row. Both flow left to right
// and wrap on a narrow terminal; every span remembers its row, so the mouse
// finds it wherever it landed.
func (s *screen) controls() {
	line, x := "", leftPad
	flush := func() {
		if line != "" {
			s.add("  " + strings.TrimRight(line, " "))
			line, x = "", leftPad
		}
	}
	put := func(plain, shown, key, alt string, gap int) {
		if line != "" && s.width > 0 && x+len([]rune(plain)) > s.width-1 {
			flush()
		}
		s.g.ctl = append(s.g.ctl, seg{x, x + len([]rune(plain)), key, alt, len(s.lines)})
		line += shown + strings.Repeat(" ", gap)
		x += len([]rune(plain)) + gap
	}
	for _, t := range s.n.toggles() { // explicit ON/off so a keypress visibly flips one; live says what it is doing
		on := t.on()
		sw, st := "○ off", dim
		if on {
			sw, st = "● on", green
		} else if t.offRed {
			st = red
		}
		plain, shown := t.label+" "+sw, hot(t.label, t.key)+" "+st.Render(sw)
		if on && t.live != nil {
			live := t.live()
			plain, shown = plain+" "+live, shown+" "+dim.Render(live)
		}
		put(plain, shown, t.key, "", 4)
	}
	flush()
	action := func(keys, word, key, alt string) { // arrow/sign keys shown in front; letters lit inside the word
		if keys != "" {
			put(keys+" "+word, keySt.Render(keys)+" "+word, key, alt, 3)
		} else {
			put(word, hot(word, key), key, alt, 3)
		}
	}
	action("↑/↓", "pick", "down", "up")
	action("←/→", "volume", "right", "left")
	action("+/-", "bitrate", "+", "-")
	action("", "input", "i", "")
	action("", "output", "o", "")
	action("", "copy", "c", "")
	action("", "chat", "t", "")
	action("", "quit", "q", "")
	flush()
	s.add("")
}

// panels are the device and tuning tiles. They change rarely and bordered
// boxes are the most expensive thing to lay out, so their lines are cached
// and rebuilt only when something in them changes; the click geometry is
// kept relative to the block and shifted to wherever it lands this frame.
func (s *screen) panels() {
	in, out := s.n.deviceLists()
	knobs := s.n.knobs()
	vals := make([]string, len(knobs))
	for i, k := range knobs {
		vals[i] = fmt.Sprint(k.get())
	}
	key := fmt.Sprintf("%d|%d|%s|%s|%s|%s|%s", s.width, s.tune, strings.Join(in, "\x00"), strings.Join(out, "\x00"),
		s.n.audio.mic, s.n.audio.out, strings.Join(vals, ","))
	c := s.cache
	if c.key != key {
		c.build(s.model, knobs)
		c.key = key
	}
	top := len(s.lines)
	s.lines = append(s.lines, c.lines...)
	s.g.dev, s.g.tune, s.g.arrows = c.dev, c.tune, c.arrows
	s.g.dev[0].top += top
	s.g.dev[1].top += top
	s.g.tune.top += top
}

// bottom is the notice, then the chat filling every row left, newest last,
// the line being typed, and the log path as the last line.
func (s *screen) bottom() {
	if s.notice != "" && s.frame-s.noticeAt < 60 {
		s.clipped("  " + yellow.Render("▸ "+s.notice))
	} else {
		s.add("")
	}
	width := 100
	if s.width > leftPad+20 {
		width = s.width - leftPad
	}
	var entry []string // the line being typed, wrapped
	if s.typing {
		entry = wrap(s.input, width-2)
		for i := range entry {
			lead := "  "
			if i == 0 {
				lead = keySt.Render("> ")
			}
			entry[i] = "  " + lead + entry[i]
		}
		entry[len(entry)-1] += selSt.Render("▏") + dim.Render("   enter sends · alt+enter new line · esc")
	} else {
		entry = []string{"  " + dim.Render("t to chat · full log: "+s.logPath)}
	}
	// Chat: every message wrapped at the terminal width, continuation lines
	// indented under the text; only the last rows that fit are shown.
	var rows []string
	for _, c := range s.n.chat.tail(60) {
		who, text := "system", c.Text
		if c.From != "" {
			who = c.From
		}
		head := c.At.Format("15:04") + " " + who + " "
		indent := strings.Repeat(" ", len([]rune(head)))
		for i, l := range wrap(text, width-len([]rune(head))) {
			if c.From == "" {
				l = dim.Render(l)
			}
			if i == 0 {
				name := bold.Render(who)
				if c.From == "" {
					name = dim.Render(who)
				}
				rows = append(rows, "  "+dim.Render(c.At.Format("15:04"))+" "+name+" "+l)
			} else {
				rows = append(rows, "  "+indent+l)
			}
		}
	}
	show := 5
	if s.height > 0 {
		show = max(0, s.height-len(s.lines)-len(entry))
	}
	s.lines = append(s.lines, rows[max(0, len(rows)-show):]...)
	s.lines = append(s.lines, entry...)
}

// wrap breaks text into lines no wider than width, at spaces where it can
// and mid-word where it must; explicit line breaks are kept.
func wrap(text string, width int) []string {
	width = max(width, 8)
	var out []string
	for _, para := range strings.Split(text, "\n") {
		line := ""
		for _, word := range strings.Split(para, " ") {
			for len([]rune(word)) > width { // a word wider than the line
				if line != "" {
					out, line = append(out, line), ""
				}
				r := []rune(word)
				out, word = append(out, string(r[:width])), string(r[width:])
			}
			switch {
			case line == "":
				line = word
			case len([]rune(line))+1+len([]rune(word)) <= width:
				line += " " + word
			default:
				out, line = append(out, line), word
			}
		}
		out = append(out, line)
	}
	return out
}

// statusBar sums the call up in one line: what we send, what comes in, the
// worst ping and jitter, a quality grade from recent drops, and the drop
// counters themselves. Per-person detail stays on the tiles.
func (m model) statusBar() string {
	people := m.n.people()
	if len(people) == 0 {
		return dim.Render(version + " · tx 0 kbps · rx 0 kbps · nobody to talk to yet")
	}
	var rx, bad float64
	var rtt, jit int64
	var lost, stall, skip, rebuf uint64
	relayed := 0
	for _, p := range people {
		if v := m.views[p]; v != nil {
			rx += v.rate.kbps
			bad = max(bad, v.rate.bad)
		}
		rtt = max(rtt, p.rttUS.Load())
		jit = max(jit, p.jitUS.Load())
		lost += p.jb.lost.Load()
		stall += p.jb.stall.Load()
		skip += p.jb.skip.Load()
		rebuf += p.jb.rebuf.Load()
		if !p.direct() && p.via.Load() != nil {
			relayed++
		}
	}
	tx := int(m.n.ctl.bitrate.Load()) * len(people)
	if m.n.ctl.silenced() {
		tx = 0
	}
	parts := []string{dim.Render(version), fmt.Sprintf("tx %d kbps", tx), fmt.Sprintf("rx %.0f kbps", rx),
		fmt.Sprintf("ping %.0f ms", float64(rtt)/1000), fmt.Sprintf("jitter %.1f ms", float64(jit)/1000), "voice " + quality(bad),
		dim.Render(fmt.Sprintf("drops %d (lost %d · stall %d · skip %d · rebuf %d)", lost+stall+skip+rebuf, lost, stall, skip, rebuf))}
	if relayed > 0 {
		parts = append(parts, yellow.Render(fmt.Sprintf("%d via relay", relayed)))
	}
	return strings.Join(parts, dim.Render(" · "))
}

func (m model) selfTile(w int) string {
	ctl := m.n.ctl
	head := "" // the name already says "(you)"; only a mute or push-to-talk needs a word
	switch {
	case ctl.muted.Load():
		head = red.Render("● MUTED")
	case ctl.ptt.Load() && ctl.talking():
		head = green.Render("● TALK")
	case ctl.ptt.Load():
		head = dim.Render("○ hold space")
	}
	talk, foot := talkText(m.n.talkMS.Load()), dim.Render(fmt.Sprintf("tx %d kbps", ctl.bitrate.Load()))
	lit, hold := m.mic.cells(barWidth(w))
	key := fmt.Sprint(w, head, lit, hold, m.mic.band(), talk, foot)
	return m.cache.self.get(key, func() string { return tile(tileMeSt, w, m.n.name+" (you)", head, m.mic, talk, foot) })
}

func (m model) peerTile(p *peer, i, w int) string {
	st := tileSt
	if i == m.cursor {
		st = tileSelSt
	}
	status := yellow.Render(spinner[m.frame%len(spinner)] + " connecting")
	if p.connected() {
		path := map[bool]string{true: "◐", false: "●"}[p.via.Load() != nil]
		status = green.Render(path) + " " + dim.Render(rttText(p))
		switch at := p.stateAt.Load(); {
		case p.silentFor() > noReplyAfter:
			status = red.Render(fmt.Sprintf("⚠ no reply %.0fs", p.silentFor().Seconds()))
		case at != 0 && time.Since(time.Unix(0, at)) < noReplyAfter && !p.hearsUs.Load(): // their fresh "no": we talk into the void
			status = red.Render("⚠ can't hear you") + " " + status
		case p.muted.Load():
			status = red.Render("muted") + " " + status
		}
		if p.sharingNow() {
			status = green.Render("▶ sharing") + " " + status
		}
	}
	v := m.views[p]
	if v == nil {
		v = &view{} // first frame: rendered uncached
	}
	// Volume as a slider: ten cells for 0–100 %, the number says the rest.
	pct := int(p.volume.Load())
	lit := min(10, (pct+5)/10)
	bar := green.Render(strings.Repeat("▮", lit)) + dim.Render(strings.Repeat("▯", 10-lit))
	vol := fmt.Sprintf("%s %3d%%", bar, pct)
	if i == m.cursor {
		vol = selSt.Render("◂ ") + vol + selSt.Render(" ▸")
	} else {
		vol = "  " + vol + "  "
	}
	if v.rate.kbps > 0 {
		vol += dim.Render(fmt.Sprintf("  %.0f kbps", v.rate.kbps))
	}
	head, talk := dim.Render(verText(p))+" "+status, talkText(p.talkMS.Load())
	lit, hold := v.meter.cells(barWidth(w))
	key := fmt.Sprint(w, i == m.cursor, p.name, head, lit, hold, v.meter.band(), talk, vol)
	return v.tile.get(key, func() string { return tile(st, w, p.name, head, v.meter, talk, vol) })
}

// mouse maps clicks and wheel onto actions using the exact drawn geometry.
func (m model) mouse(e tea.MouseMsg) (tea.Model, tea.Cmd) {
	wheel := e.Button == tea.MouseButtonWheelUp || e.Button == tea.MouseButtonWheelDown
	dir := map[bool]int{true: +1, false: -1}[e.Button == tea.MouseButtonWheelUp]
	if !wheel && e.Action != tea.MouseActionPress {
		return m, nil // releases and drags: nothing to hit-test, no render
	}
	_, g := m.render()
	if t := g.tune; !wheel && e.Y >= t.top+2 && e.Y < t.top+2+t.rows && e.X >= t.x0 && e.X < t.x1 {
		m.tune = e.Y - t.top - 2 // only the arrows turn a knob; the wheel is too easy to nudge by accident
		switch {
		case e.X <= g.arrows[m.tune][0]+1: // on or next to ◂
			return m.turn(-1)
		case e.X >= g.arrows[m.tune][1]-1: // on or next to ▸
			return m.turn(+1)
		}
		return m, nil
	}
	in, out := m.n.deviceLists()
	if !wheel { // a device row?
		for i, d := range g.dev {
			row := e.Y - d.top - 2
			if row < 0 || row >= d.rows || e.X < d.x0 || e.X >= d.x1 {
				continue
			}
			if i == 0 {
				return m.choose(malgo.Capture, in[row])
			}
			return m.choose(malgo.Playback, out[row])
		}
	}
	if i, ok := g.tileAt(e.X, e.Y); ok {
		switch {
		case wheel && i == 0:
			return m.nudgeBitrate(dir)
		case wheel:
			m.cursor = i - 1
			return m.nudgeVolume(dir)
		case i == 0:
			return m.act("m")
		default:
			m.cursor = i - 1
			return m, nil
		}
	}
	if wheel {
		return m, nil
	}
	if e.Y == g.linkRow {
		return m.act("c")
	}
	for _, s := range g.ctl { // toggles, actions, update prompt: right-click takes alt
		if e.Y == s.row && e.X >= s.x0 && e.X < s.x1 {
			if e.Button == tea.MouseButtonRight && s.alt != "" {
				return m.act(s.alt)
			}
			return m.act(s.key)
		}
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
	tileMinW  = 34
	tileExtra = 14
)

var (
	tileSt    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	tileSelSt = tileSt.BorderForeground(lipgloss.Color("81"))
	tileMeSt  = tileSt.BorderForeground(lipgloss.Color("42"))
)

// Speech levels (meter 0..1) at which a tile's border turns into dots of
// growing weight, fading back with the meter.
const (
	speakLvl = 0.2
	loudLvl  = 0.45
	peakLvl  = 0.7
)

// tile draws one card: name + status, the VU bar with talk time at its
// right, and a footer line — inside a border that reacts to the voice.
func tile(st lipgloss.Style, w int, name, status string, mt meter, talk, foot string) string {
	nameSt := bold
	if mt.band() > 0 {
		nameSt = green.Bold(true) // the one talking stands out by name too
	}
	head := nameSt.Render(trunc(name, w-4))
	if status != "" {
		head += " " + status
	}
	talk = fmt.Sprintf("%8s", talk)
	// While they speak the frame itself turns into dots that grow with the
	// level (· ∙ •); silence brings the plain rounded border back.
	switch mt.band() {
	case 3:
		st = st.Border(dotBorder("•")).BorderForeground(lipgloss.Color("46"))
	case 2:
		st = st.Border(dotBorder("∙")).BorderForeground(lipgloss.Color("42"))
	case 1:
		st = st.Border(dotBorder("·")).BorderForeground(lipgloss.Color("35"))
	}
	return st.Width(w).Render(fmt.Sprintf("%s\n%s %s\n%s", head, mt.bar(barWidth(w)), dim.Render(talk), foot))
}

// barWidth is the VU bar's cells in a w-wide tile: borders, padding, the
// 8-char talk time and a space are taken off.
func barWidth(w int) int { return w - 6 - 8 - 1 }

// dotBorder is a border drawn entirely with one character.
func dotBorder(ch string) lipgloss.Border {
	return lipgloss.Border{Top: ch, Bottom: ch, Left: ch, Right: ch,
		TopLeft: ch, TopRight: ch, BottomLeft: ch, BottomRight: ch}
}

// panelCache holds the rendered device and tuning tiles between frames.
// It is a pointer shared by every copy of the model, so a rebuild sticks.
type panelCache struct {
	self   tileCache // our own tile, cached the same way as the others'
	key    string
	lines  []string
	dev    [2]devBox // tops relative to the first cached line
	tune   devBox
	arrows [][2]int
}

// build lays the panels out: device tiles side by side when they fit,
// stacked on a narrow terminal, the tuning tile below.
func (c *panelCache) build(m model, knobs []knob) {
	inputs, outputs := m.n.deviceLists()
	stack := m.width > 0 && m.width < 2*(tileMinW+2)+leftPad+1
	dw := max(tileMinW, min(m.width/2-leftPad-1, 60))
	if stack {
		dw = max(tileMinW, min(m.width-leftPad-2, 60))
	}
	in := deviceTile(dw, "input", "i", inputs, m.n.audio.mic)
	out := deviceTile(dw, "output", "o", outputs, m.n.audio.out)
	stride := lipgloss.Width(strings.SplitN(in, "\n", 2)[0])
	c.dev[0] = devBox{0, leftPad, leftPad + stride, len(inputs)}
	var block string
	if stack {
		c.dev[1] = devBox{lipgloss.Height(in), leftPad, leftPad + stride, len(outputs)}
		block = lipgloss.JoinVertical(lipgloss.Left, in, out)
	} else {
		c.dev[1] = devBox{0, leftPad + stride, leftPad + 2*stride, len(outputs)}
		block = lipgloss.JoinHorizontal(lipgloss.Top, in, out)
	}
	c.lines = strings.Split(lipgloss.NewStyle().PaddingLeft(leftPad).Render(block), "\n")

	// Tuning tile: the echo canceller knobs, one per row, saved as they turn.
	rows := []string{bold.Render("tuning") + dim.Render("   tab picks · [ ] or click ◂ ▸")}
	c.arrows = c.arrows[:0]
	for i, k := range knobs {
		val := fmt.Sprintf("◂ %d %s ▸", k.get(), k.unit)
		pad := strings.Repeat(" ", max(1, dw-2-len([]rune(k.name))-len([]rune(val))))
		left := leftPad + 2 + len([]rune(k.name)) + len(pad) // content starts after border + padding
		c.arrows = append(c.arrows, [2]int{left, left + len([]rune(val)) - 1})
		if i == m.tune {
			rows = append(rows, k.name+pad+selSt.Render(val))
		} else {
			rows = append(rows, dim.Render(k.name+pad+val))
		}
	}
	c.tune = devBox{len(c.lines), leftPad, leftPad + stride, len(knobs)}
	c.lines = append(c.lines, strings.Split(lipgloss.NewStyle().PaddingLeft(leftPad).Render(tileSt.Width(dw).Render(strings.Join(rows, "\n"))), "\n")...)
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
