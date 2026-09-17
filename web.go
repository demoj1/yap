package main

import (
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
)

// The browser UI: the same state and switches as the terminal, served on
// localhost only. The page gets a JSON snapshot ten times a second over
// SSE and posts actions back.

//go:embed web/index.html
var webPage []byte

const webPort = 7333 // the usual address; another yap on the machine gets a free port

type webServer struct {
	n   *node
	mu  sync.Mutex
	ln  net.Listener
	srv *http.Server
}

func (w *webServer) running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ln != nil
}

func (w *webServer) url() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ln == nil {
		return ""
	}
	return "http://" + w.ln.Addr().String()
}

// start serves the page; the notice says where.
func (w *webServer) start() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ln != nil {
		return "web on — http://" + w.ln.Addr().String()
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", webPort))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return "web: " + err.Error()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		if page, err := os.ReadFile("web/index.html"); err == nil { // the working copy when run from the repo: edit, reload, no rebuild
			rw.Write(page)
			return
		}
		rw.Write(webPage)
	})
	mux.HandleFunc("/events", w.events)
	mux.HandleFunc("/act", w.act)
	mux.HandleFunc("/upload", w.upload)
	mux.HandleFunc("/video", w.videoIn)
	mux.HandleFunc("/video/", w.videoOut)
	mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(w.n.files.dir))))
	w.ln, w.srv = ln, &http.Server{Handler: mux}
	go w.srv.Serve(ln)
	w.n.system("web UI at http://%s", ln.Addr())
	return "web on — http://" + ln.Addr().String()
}

func (w *webServer) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.srv != nil {
		w.srv.Close()
		w.srv, w.ln = nil, nil
	}
}

// events streams a snapshot ten times a second for as long as the page listens.
func (w *webServer) events(rw http.ResponseWriter, r *http.Request) {
	fl, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "no streaming", 500)
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	enc := json.NewEncoder(rw)
	for {
		rw.Write([]byte("data: "))
		enc.Encode(w.n.snapshot())
		rw.Write([]byte("\n"))
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// upload takes one file (?name=, raw body) and sends it to the room.
func (w *webServer) upload(rw http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, fileMax))
	if err == nil {
		err = w.n.sendFile(r.URL.Query().Get("name"), data)
	}
	if err != nil {
		http.Error(rw, err.Error(), 400)
	}
}

// videoIn takes one encoded frame of our screen from the page (?key=1 for
// a key frame) and sends it to everyone.
func (w *webServer) videoIn(rw http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, 4<<20))
	if err != nil {
		http.Error(rw, err.Error(), 400)
		return
	}
	w.n.sendVideo(data, r.URL.Query().Get("key") == "1")
}

// videoOut streams someone's screen to the page as it comes in:
// [len u32][flags u8][frame] per frame, for as long as the page reads.
func (w *webServer) videoOut(rw http.ResponseWriter, r *http.Request) {
	p := w.n.peerByName(strings.TrimPrefix(r.URL.Path, "/video/"))
	if p == nil {
		http.NotFound(rw, r)
		return
	}
	rx := p.video.Load()
	if rx == nil {
		rx = newVideoRx()
		p.video.Store(rx)
	}
	fl, _ := rw.(http.Flusher)
	rw.Header().Set("Content-Type", "application/octet-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	head := make([]byte, 5)
	for len(rx.out) > 0 { // whatever piled up before the page started watching is stale
		<-rx.out
	}
	w.n.sendTo(p, videoCtl(videoFlagWant)) // frames come only while someone says they watch
	beat := time.NewTicker(time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			w.n.sendTo(p, videoCtl(videoFlagWant))
		case f := <-rx.out:
			binary.BigEndian.PutUint32(head, uint32(len(f.data)))
			head[4] = 0
			if f.key {
				head[4] = videoFlagKey
			}
			if _, err := rw.Write(head); err != nil {
				return
			}
			if _, err := rw.Write(f.data); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

// act performs one action from the page and answers with the notice.
func (w *webServer) act(rw http.ResponseWriter, r *http.Request) {
	var a struct {
		Key     string
		Peer    string
		Volume  *int
		MicGain *int
		Device  *struct{ Kind, Name string }
		Knob    *struct{ I, Dir int }
		Bitrate int
		Chat    string
		Theme   string
		Share   *bool
		Join    string // a link: leave this room and restart into that one
	}
	if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&a) != nil {
		http.Error(rw, "bad request", 400)
		return
	}
	n := w.n
	notice := ""
	switch {
	case a.Key != "":
		notice = n.press(a.Key)
	case a.Volume != nil:
		if p := n.peerByName(a.Peer); p != nil {
			v := max(0, min(200, *a.Volume))
			n.setVolume(p, v)
			notice = fmt.Sprintf("%s volume %d%%", p.name, v)
		}
	case a.MicGain != nil:
		g := max(0, min(300, *a.MicGain))
		n.ctl.micGain.Store(int32(g))
		n.set.MicGain = g
		n.set.save()
		notice = fmt.Sprintf("mic gain %d%%", g)
	case a.Device != nil:
		kind := malgo.Capture
		if a.Device.Kind == "out" {
			kind = malgo.Playback
		}
		notice = n.useDevice(kind, a.Device.Name)
	case a.Knob != nil:
		notice = n.turnKnob(a.Knob.I, a.Knob.Dir)
	case a.Bitrate != 0:
		notice = n.nudgeBitrate(a.Bitrate)
	case a.Chat != "":
		n.say(a.Chat)
	case a.Theme != "":
		n.set.Theme = a.Theme
		n.set.save()
	case a.Share != nil:
		n.ctl.sharing.Store(*a.Share)
		n.sendState()
	case a.Join != "":
		l, err := parseLink(strings.TrimSpace(a.Join))
		if err != nil {
			notice = "that is not a yap link"
			break
		}
		if l.String() == n.link.String() {
			notice = "you are already in that room"
			break
		}
		s := l.String()
		n.rejoin.Store(&s)
		notice = "joining " + s + " — the page reconnects in a moment"
		if n.stop != nil {
			go n.stop()
		}
	}
	json.NewEncoder(rw).Encode(map[string]string{"notice": notice})
}

func (n *node) peerByName(name string) *peer {
	for _, p := range n.people() {
		if p.name == name {
			return p
		}
	}
	return nil
}

// snapshot is everything the page shows, in plain numbers; the page does
// the styling.
type snapshot struct {
	Version, Link, Name, Update string
	Theme                       string // "light", "dark" or "" for the system's
	Self                        selfJSON
	People, Relays              []peerJSON
	Toggles                     []toggleJSON
	Knobs                       []knobJSON
	Inputs, Outputs             []string
	Mic, Out                    string
	Bitrate                     int32
	Logs                        []string
	Chat                        []chatJSON
}

type chatJSON struct {
	At   int64 // unix ms
	From string
	Text string
	File string // served at /files/<File>
	Size int
}

type selfJSON struct {
	Level               float64 // dBFS, -Inf when silent
	Talk                int64   // ms
	Gain                int     // mic gain, percent
	KeyReq              uint32  // bumped when a viewer wants a key frame of our screen
	Watchers            int     // people our screen goes to right now; 0 means don't bother encoding
	Muted, PTT, Talking bool
}

type peerJSON struct {
	Name, Ver, Path          string
	Connected                bool
	RTT                      float64 // ms
	Jitter                   float64 // ms
	RxBytes                  uint64
	Level                    float64 // dBFS
	Talk                     int64   // ms
	Volume                   int32
	Muted, CantHear, Sharing bool
	NoReply                  float64 // s, 0 when fine
	Lost, Stall, Skip, Rebuf uint64
}

type toggleJSON struct {
	Key, Label, Live string
	On, OffRed       bool
}

type knobJSON struct {
	Name, Unit string
	Value      int
}

func (n *node) snapshot() snapshot {
	s := snapshot{Version: version, Link: n.link.String(), Name: n.name, Bitrate: n.ctl.bitrate.Load(),
		Mic: n.audio.mic, Out: n.audio.out, Theme: n.set.Theme}
	if tag := n.update.Load(); tag != nil {
		s.Update = *tag
	}
	s.Self = selfJSON{Level: dbJSON(n.micDB.Load()), Talk: n.talkMS.Load(), Gain: int(n.ctl.micGain.Load()), KeyReq: n.keyReq.Load(), Watchers: n.watchers(), Muted: n.ctl.muted.Load(), PTT: n.ctl.ptt.Load(), Talking: n.ctl.talking()}
	for _, p := range n.peerList() {
		j := peerJSON{Name: p.name, Ver: verText(p), Connected: p.connected(), Path: "connecting",
			RTT: float64(p.rttUS.Load()) / 1000, Jitter: float64(p.jitUS.Load()) / 1000, RxBytes: p.rxBytes.Load(),
			Level: dbJSON(p.levelDB.Load()), Talk: p.talkMS.Load(), Volume: p.volume.Load(), Muted: p.muted.Load(), Sharing: p.sharingNow(),
			Lost: p.jb.lost.Load(), Stall: p.jb.stall.Load(), Skip: p.jb.skip.Load(), Rebuf: p.jb.rebuf.Load()}
		switch {
		case p.direct():
			j.Path = "direct"
		case p.via.Load() != nil:
			j.Path = "via " + p.via.Load().name
		}
		if p.connected() {
			if silent := p.silentFor(); silent > noReplyAfter {
				j.NoReply = silent.Seconds()
			}
			if at := p.stateAt.Load(); at != 0 && time.Since(time.Unix(0, at)) < noReplyAfter && !p.hearsUs.Load() {
				j.CantHear = true
			}
		}
		if p.relay {
			if a := p.addr.Load(); a != nil {
				j.Path = a.String()
			}
			s.Relays = append(s.Relays, j)
		} else {
			s.People = append(s.People, j)
		}
	}
	for _, t := range n.toggles() {
		j := toggleJSON{Key: t.key, Label: t.label, On: t.on(), OffRed: t.offRed}
		if j.On && t.live != nil {
			j.Live = t.live()
		}
		s.Toggles = append(s.Toggles, j)
	}
	for _, k := range n.knobs() {
		s.Knobs = append(s.Knobs, knobJSON{k.name, k.unit, k.get()})
	}
	s.Inputs, s.Outputs = n.deviceLists()
	if n.logs != nil {
		s.Logs = n.logs.tail(40)
	}
	for _, c := range n.chat.tail(100) {
		s.Chat = append(s.Chat, chatJSON{c.At.UnixMilli(), c.From, c.Text, c.File, c.Size})
	}
	return s
}

// dbJSON turns a stored level into a JSON-safe number (-Inf is not one).
func dbJSON(bits uint64) float64 { return max(-100, math.Float64frombits(bits)) }

// logRing keeps the last log lines for the page; stats lines stay in the file.
type logRing struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRing) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if !strings.Contains(line, ": tx ") {
		l.mu.Lock()
		l.lines = append(l.lines, line)
		if len(l.lines) > logKeep {
			l.lines = l.lines[len(l.lines)-logKeep:]
		}
		l.mu.Unlock()
	}
	return len(p), nil
}

func (l *logRing) tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines[max(0, len(l.lines)-n):]...)
}

// openBrowser shows url in the default browser; on Windows, where a
// terminal is a poor home, the page is the way to use yap.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Println("browser:", err)
	}
}
