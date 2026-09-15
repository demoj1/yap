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

const (
	devRefresh   = 150             // frames (~5 s) between device list refreshes
	noReplyAfter = 3 * time.Second // a connected peer silent this long is flagged on its tile
)

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

// knob is one tunable number on the tuning tile.
type knob struct {
	name, unit   string
	get          func() int
	set          func(int)
	step, lo, hi int
}

// knobs lists the echo canceller settings the tuning tile edits; every
// change is saved and applied live.
func (m model) knobs() []knob {
	s := m.n.set
	apply := func() {
		s.save()
		m.n.audio.setAEC(s.AECTail, s.AECSuppress, s.AECSuppressActive)
	}
	return []knob{
		{"echo tail", "ms", func() int { return s.AECTail }, func(v int) { s.AECTail = v; apply() }, 25, 100, 600},
		{"echo suppress", "dB", func() int { return s.AECSuppress }, func(v int) { s.AECSuppress = v; apply() }, 5, -80, -10},
		{"echo suppress while they talk", "dB", func() int { return s.AECSuppressActive }, func(v int) { s.AECSuppressActive = v; apply() }, 5, -50, -5},
	}
}

// turn nudges the selected knob by dir steps within its range.
func (m model) turn(dir int) (tea.Model, tea.Cmd) {
	k := m.knobs()[m.tune]
	v := max(k.lo, min(k.hi, k.get()+dir*k.step))
	k.set(v)
	return m.note(fmt.Sprintf("%s %d %s", k.name, v, k.unit)), nil
}

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
	tune     int            // selected row of the tuning tile
	seen     map[*peer]bool // people heard from at least once: a new one chimes in, a vanished one chimes out
	asked    bool           // the update dialog was answered (either way)
	doUpdate bool           // the answer was yes: main updates and restarts after the TUI exits
	logs     []string
	frame    int
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

// newUI builds the screen; notice, if any, is shown for the first ~10 s.
func newUI(n *node, logPath, notice string) *ui {
	u := &ui{}
	m := model{n: n, logPath: logPath, meters: map[*peer]*meter{}, rates: map[*peer]*rate{}, seen: map[*peer]bool{},
		notice: notice, noticeAt: 240} // a notice lives 60 frames past noticeAt: this one until frame 300
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

// Write feeds log lines to the screen. The periodic per-peer stats stay in
// the file only: the status bar and tiles show them live, and on screen
// they would bury the events that matter (who joined, how, who left).
func (u *ui) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if !strings.Contains(line, ": tx ") {
		u.prog.Send(logMsg(line))
	}
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
			r.feed(p.rxBytes.Load(), drops(p), now)
		}
		for p := range m.meters {
			if !alive[p] {
				delete(m.meters, p)
				delete(m.rates, p)
			}
		}
		for _, p := range peers { // chimes: a person arriving or leaving
			if !p.relay && p.connected() && !m.seen[p] {
				m.seen[p] = true
				m.n.cue(cueJoin)
			}
		}
		for p := range m.seen {
			if !alive[p] {
				delete(m.seen, p)
				m.n.cue(cueLeave)
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
		m.n.sendState()
		return m.note("mic " + map[bool]string{true: "MUTED", false: "on"}[ctl.muted.Load()]), nil
	case "p":
		ctl.ptt.Store(!ctl.ptt.Load())
		m.n.set.PTT = ctl.ptt.Load()
		m.n.set.save()
		m.n.sendState()
		if ctl.ptt.Load() {
			return m.note("push-to-talk on — hold space to speak"), nil
		}
		return m.note("push-to-talk off — mic is open"), nil
	case " ":
		if ctl.ptt.Load() {
			wasTalking := ctl.talking()
			ctl.pressTalk()
			if !wasTalking {
				m.n.sendState()
			}
		}
	case "c":
		copyToClipboard(m.n.link.String())
		return m.note("link copied"), nil
	case "tab":
		m.tune = (m.tune + 1) % len(m.knobs())
	case "[":
		return m.turn(-1)
	case "]":
		return m.turn(+1)
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
	ctl := m.n.ctl
	var g geometry
	var lines []string
	clip := lipgloss.NewStyle().MaxWidth(max(1, m.width)) // a line that wrapped would shift every row below it
	add := func(s string) {
		if m.width > 0 {
			s = clip.Render(s)
		}
		lines = append(lines, s)
	}

	add("")
	g.linkRow = len(lines)
	add("  " + linkSt.Render(m.n.link.String()) + dim.Render("   c to copy"))
	if tag := m.n.update.Load(); tag != nil {
		if m.asked {
			add("  " + dim.Render(*tag+" is out — yap update"))
		} else { // the dialog: y updates and restarts into the same room, n dismisses
			lead := fmt.Sprintf("⬆ %s available (you run %s) — update now?   ", *tag, version)
			yes, no := "[y] yes", "[n] later"
			x, row := leftPad+len([]rune(lead)), len(lines)
			g.ctl = append(g.ctl, seg{x, x + len(yes), "y", "", row}, seg{x + len(yes) + 3, x + len(yes) + 3 + len(no), "n", "", row})
			add("  " + yellow.Render(lead) + hotSt.Render(yes) + "   " + hotSt.Render(no))
		}
	}
	peers := m.people()
	add("  " + m.statusBar(peers))
	add("")
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
		add("  " + keySt.Render("⇄ "+r.name) + " " + dim.Render(verText(r)) + " " + state)
	}
	add("")

	// Toggles and actions flow left to right and wrap on a narrow terminal;
	// every span remembers its row, so the mouse finds it wherever it landed.
	line, x := "", leftPad
	flush := func() {
		if line != "" {
			add("  " + strings.TrimRight(line, " "))
			line, x = "", leftPad
		}
	}
	put := func(plain, shown, key, alt string, gap int) {
		if line != "" && m.width > 0 && x+len([]rune(plain)) > m.width-1 {
			flush()
		}
		g.ctl = append(g.ctl, seg{x, x + len([]rune(plain)), key, alt, len(lines)})
		line += shown + strings.Repeat(" ", gap)
		x += len([]rune(plain)) + gap
	}
	// Toggles: always visible with explicit ON/off, so a keypress visibly
	// flips one; live shows what the switch is doing right now.
	chip := func(key, name string, on, offRed bool, live string) {
		sw, st := "○ off", dim
		if on {
			sw, st = "● on", green
		} else if offRed {
			st = red
		}
		plain, shown := name+" "+sw, hot(name, key)+" "+st.Render(sw)
		if on && live != "" {
			plain, shown = plain+" "+live, shown+" "+dim.Render(live)
		}
		put(plain, shown, key, "", 4)
	}
	gateLive := "shut"
	if m.n.gateOpen.Load() {
		gateLive = "open"
	}
	chip("m", "mic", !ctl.muted.Load(), true, "")
	chip("d", "denoise", ctl.denoise.Load(), false, "")
	chip("g", "gate", ctl.gate.Load(), false, gateLive)
	chip("e", "echo", ctl.aec.Load(), false, fmt.Sprintf("−%.0f dB", max(0, math.Float64frombits(m.n.audio.aecDB.Load()))))
	chip("a", "gain", ctl.agc.Load(), false, fmt.Sprintf("×%.1f", math.Float64frombits(m.n.agcGain.Load())))
	chip("l", "lock", m.n.locked.Load(), false, "")
	chip("p", "ptt", ctl.ptt.Load(), false, "")
	flush()

	// Actions: arrow/sign ones show their keys in front; letter ones light
	// the letter inside the word, like the toggles above.
	action := func(keys, word, key, alt string) {
		plain, shown := word, hot(word, key)
		if keys != "" {
			plain, shown = keys+" "+word, keySt.Render(keys)+" "+word
		}
		put(plain, shown, key, alt, 3)
	}
	action("↑/↓", "pick", "down", "up")
	action("←/→", "volume", "right", "left")
	action("+/-", "bitrate", "+", "-")
	action("", "input", "i", "")
	action("", "output", "o", "")
	action("", "copy", "c", "")
	action("", "quit", "q", "")
	flush()
	add("")

	// Device tiles: every microphone and speaker listed, the one in use
	// marked; a click on a row switches to it. Side by side when they fit,
	// stacked on a narrow terminal.
	stack := m.width > 0 && m.width < 2*(tileMinW+2)+leftPad+1
	dw := max(tileMinW, min(m.width/2-leftPad-1, 60))
	if stack {
		dw = max(tileMinW, min(m.width-leftPad-2, 60))
	}
	in := deviceTile(dw, "input", "i", m.inputs, m.n.audio.mic)
	out := deviceTile(dw, "output", "o", m.outputs, m.n.audio.out)
	stride := lipgloss.Width(strings.SplitN(in, "\n", 2)[0])
	g.dev[0] = devBox{len(lines), leftPad, leftPad + stride, len(m.inputs)}
	var block string
	if stack {
		g.dev[1] = devBox{len(lines) + lipgloss.Height(in), leftPad, leftPad + stride, len(m.outputs)}
		block = lipgloss.JoinVertical(lipgloss.Left, in, out)
	} else {
		g.dev[1] = devBox{len(lines), leftPad + stride, leftPad + 2*stride, len(m.outputs)}
		block = lipgloss.JoinHorizontal(lipgloss.Top, in, out)
	}
	for _, ln := range strings.Split(lipgloss.NewStyle().PaddingLeft(leftPad).Render(block), "\n") {
		add(ln)
	}

	// Tuning tile: the echo canceller knobs, one per row, saved as they turn.
	knobs := m.knobs()
	rows := []string{bold.Render("tuning") + dim.Render("   tab picks · [ ] or click ◂ ▸")}
	g.arrows = g.arrows[:0]
	for i, k := range knobs {
		val := fmt.Sprintf("◂ %d %s ▸", k.get(), k.unit)
		pad := strings.Repeat(" ", max(1, dw-2-len([]rune(k.name))-len([]rune(val))))
		left := leftPad + 2 + len([]rune(k.name)) + len(pad) // content starts after border + padding
		g.arrows = append(g.arrows, [2]int{left, left + len([]rune(val)) - 1})
		if i == m.tune {
			rows = append(rows, k.name+pad+selSt.Render(val))
		} else {
			rows = append(rows, dim.Render(k.name+pad+val))
		}
	}
	g.tune = devBox{len(lines), leftPad, leftPad + stride, len(knobs)}
	for _, ln := range strings.Split(lipgloss.NewStyle().PaddingLeft(leftPad).Render(tileSt.Width(dw).Render(strings.Join(rows, "\n"))), "\n") {
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

// statusBar sums the call up in one line: what we send, what comes in, the
// worst ping and jitter, a quality grade from recent drops, and the drop
// counters themselves. Per-person detail stays on the tiles.
func (m model) statusBar(peers []*peer) string {
	var rx, bad float64
	var rtt, jit int64
	var lost, stall, skip, rebuf uint64
	relayed := 0
	for _, p := range peers {
		if r := m.rates[p]; r != nil {
			rx += r.kbps
			bad = max(bad, r.bad)
		}
		rtt = max(rtt, p.rttUS.Load())
		jit = max(jit, p.jitUS.Load())
		lost += p.jb.lost.Load()
		stall += p.jb.stall.Load()
		skip += p.jb.skip.Load()
		rebuf += p.jb.rebuf.Load()
		if p.via.Load() != nil && !p.direct() {
			relayed++
		}
	}
	tx := int(m.n.ctl.bitrate.Load()) * len(peers)
	if m.n.ctl.muted.Load() {
		tx = 0
	}
	sep := dim.Render(" · ")
	if len(peers) == 0 {
		return dim.Render(version + " · tx 0 kbps · rx 0 kbps · nobody to talk to yet")
	}
	s := fmt.Sprintf("%s%stx %d kbps%srx %.0f kbps", dim.Render(version), sep, tx, sep, rx)
	s += fmt.Sprintf("%sping %.0f ms%sjitter %.1f ms", sep, float64(rtt)/1000, sep, float64(jit)/1000)
	s += sep + "voice " + quality(bad)
	s += sep + dim.Render(fmt.Sprintf("drops %d (lost %d · stall %d · skip %d · rebuf %d)", lost+stall+skip+rebuf, lost, stall, skip, rebuf))
	if relayed > 0 {
		s += sep + yellow.Render(fmt.Sprintf("%d via relay", relayed))
	}
	return s
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
	return tile(tileMeSt, w, m.n.name+" (you)", head,
		m.mic, talkText(m.n.talkMS.Load()), dim.Render(fmt.Sprintf("tx %d kbps", ctl.bitrate.Load())))
}

func (m model) peerTile(p *peer, i, w int) string {
	st := tileSt
	if i == m.cursor {
		st = tileSelSt
	}
	status := yellow.Render(spinner[m.frame%len(spinner)] + " connecting")
	if silent := p.silentFor(); p.connected() && silent > noReplyAfter {
		status = red.Render(fmt.Sprintf("⚠ no reply %.0fs", silent.Seconds()))
	} else if p.connected() {
		path := "●"
		if p.via.Load() != nil {
			path = "◐"
		}
		status = green.Render(path) + " " + dim.Render(rttText(p))
		if p.muted.Load() {
			status = red.Render("muted") + " " + status
		}
		// They keep telling us whether our packets reach them; a fresh "no" means we talk into the void.
		if at := p.stateAt.Load(); at != 0 && time.Since(time.Unix(0, at)) < noReplyAfter && !p.hearsUs.Load() {
			status = red.Render("⚠ can't hear you") + " " + status
		}
	}
	var mt meter
	if x := m.meters[p]; x != nil {
		mt = *x
	}
	// Volume as a slider: ten cells for 0–100 %, the number says the rest.
	v := int(p.volume.Load())
	lit := min(10, (v+5)/10)
	bar := green.Render(strings.Repeat("▮", lit)) + dim.Render(strings.Repeat("▯", 10-lit))
	vol := fmt.Sprintf("%s %3d%%", bar, v)
	if i == m.cursor {
		vol = selSt.Render("◂ ") + vol + selSt.Render(" ▸")
	} else {
		vol = "  " + vol + "  "
	}
	if r := m.rates[p]; r != nil && r.kbps > 0 {
		vol += dim.Render(fmt.Sprintf("  %.0f kbps", r.kbps))
	}
	return tile(st, w, p.name, dim.Render(verText(p))+" "+status, mt, talkText(p.talkMS.Load()), vol)
}

// mouse maps clicks and wheel onto actions using the exact drawn geometry.
func (m model) mouse(e tea.MouseMsg) (tea.Model, tea.Cmd) {
	_, g := m.render()
	wheel := e.Button == tea.MouseButtonWheelUp || e.Button == tea.MouseButtonWheelDown
	up := e.Button == tea.MouseButtonWheelUp
	if !wheel && e.Action != tea.MouseActionPress {
		return m, nil
	}
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
	if !wheel { // a device row?
		for i, d := range g.dev {
			row := e.Y - d.top - 2
			if row < 0 || row >= d.rows || e.X < d.x0 || e.X >= d.x1 {
				continue
			}
			if i == 0 {
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
	if mt.level >= speakLvl {
		nameSt = green.Bold(true) // the one talking stands out by name too
	}
	head := nameSt.Render(trunc(name, w-4))
	if status != "" {
		head += " " + status
	}
	talk = fmt.Sprintf("%8s", talk)
	// While they speak the frame itself turns into dots that grow with the
	// level (· ∙ •); silence brings the plain rounded border back.
	switch {
	case mt.level >= peakLvl:
		st = st.Border(dotBorder("•")).BorderForeground(lipgloss.Color("46"))
	case mt.level >= loudLvl:
		st = st.Border(dotBorder("∙")).BorderForeground(lipgloss.Color("42"))
	case mt.level >= speakLvl:
		st = st.Border(dotBorder("·")).BorderForeground(lipgloss.Color("35"))
	}
	return st.Width(w).Render(fmt.Sprintf("%s\n%s %s\n%s", head, mt.bar(w-6-len(talk)-1), dim.Render(talk), foot))
}

// dotBorder is a border drawn entirely with one character.
func dotBorder(ch string) lipgloss.Border {
	return lipgloss.Border{Top: ch, Bottom: ch, Left: ch, Right: ch,
		TopLeft: ch, TopRight: ch, BottomLeft: ch, BottomRight: ch}
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
