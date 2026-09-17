package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
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
		rw.Write(webPage)
	})
	mux.HandleFunc("/events", w.events)
	mux.HandleFunc("/act", w.act)
	w.ln, w.srv = ln, &http.Server{Handler: mux}
	go w.srv.Serve(ln)
	log.Println("web UI at http://" + ln.Addr().String())
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

// act performs one action from the page and answers with the notice.
func (w *webServer) act(rw http.ResponseWriter, r *http.Request) {
	var a struct {
		Key     string
		Peer    string
		Volume  *int
		Device  *struct{ Kind, Name string }
		Knob    *struct{ I, Dir int }
		Bitrate int
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
	Self                        selfJSON
	People, Relays              []peerJSON
	Toggles                     []toggleJSON
	Knobs                       []knobJSON
	Inputs, Outputs             []string
	Mic, Out                    string
	Bitrate                     int32
	Logs                        []string
}

type selfJSON struct {
	Level               float64 // dBFS, -Inf when silent
	Talk                int64   // ms
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
	Muted, CantHear          bool
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
		Mic: n.audio.mic, Out: n.audio.out}
	if tag := n.update.Load(); tag != nil {
		s.Update = *tag
	}
	s.Self = selfJSON{Level: dbJSON(n.micDB.Load()), Talk: n.talkMS.Load(), Muted: n.ctl.muted.Load(), PTT: n.ctl.ptt.Load(), Talking: n.ctl.talking()}
	for _, p := range n.peerList() {
		j := peerJSON{Name: p.name, Ver: verText(p), Connected: p.connected(), Path: "connecting",
			RTT: float64(p.rttUS.Load()) / 1000, Jitter: float64(p.jitUS.Load()) / 1000, RxBytes: p.rxBytes.Load(),
			Level: dbJSON(p.levelDB.Load()), Talk: p.talkMS.Load(), Volume: p.volume.Load(), Muted: p.muted.Load(),
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
