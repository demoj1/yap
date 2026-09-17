package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"syscall"
)

var version = "dev" // set by -ldflags in CI

func usage() {
	fmt.Fprintf(os.Stderr, `yap — voice call from the terminal, nothing else.

  yap                             host a room: the link is copied, send it to friends
  yap listen [-p 4444] [-new]     same, explicitly (link is kept across restarts; -new makes a fresh one)
  yap join <link>                 join a friend's room
  yap relay [-p 4444] <link>      run as a headless relay hub (public, always-on box)
  yap devices                     list microphones and speakers
  yap reset                       forget saved devices/volumes, back to defaults
  yap stats                       how connections went: per friend, direct/relayed, time to connect, drops
  yap update                      replace this binary with the latest release

  common flags: -name <shown to the friend>  -mic <name>  -out <name>
                -plain (logs instead of the TUI)  -nodenoise
`)
	os.Exit(2)
}

func main() {
	log.SetFlags(log.Ltime)
	if path := os.Getenv("YAP_CPUPROFILE"); path != "" { // diagnostics: go tool pprof -top yap <path>
		f, err := os.Create(path)
		if err != nil {
			log.Fatal(err)
		}
		must(pprof.StartCPUProfile(f))
		defer pprof.StopCPUProfile()
		go func() { // a relay has no quit key: flush the profile on ctrl-c / SIGTERM too
			c := make(chan os.Signal, 1)
			signal.Notify(c, os.Interrupt, syscall.SIGTERM)
			<-c
			pprof.StopCPUProfile()
			os.Exit(0)
		}()
	}
	if len(os.Args) < 2 { // bare "yap" hosts a room: the one-command start
		os.Args = append(os.Args, "listen")
	}
	if os.Args[1] == "devices" {
		printDevices()
		return
	}
	if os.Args[1] == "stats" {
		printStats()
		return
	}
	if os.Args[1] == "update" {
		if err := selfUpdate(); err != nil {
			log.Fatal("update: ", err)
		}
		return
	}
	if os.Args[1] == "reset" {
		set := loadSettings()
		if err := os.Remove(set.path); err != nil && !os.IsNotExist(err) {
			log.Fatal(err)
		}
		fmt.Println("settings reset to defaults:", set.path)
		return
	}
	set := loadSettings()

	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	port := fs.Int("p", 4444, "UDP port (listen only)")
	rotate := fs.Bool("new", false, "forget the saved link and make a new one (listen only)")
	name := fs.String("name", defaultName(), "your name, shown to the friend")
	plain := fs.Bool("plain", false, "plain logs instead of the TUI")
	nodenoise := fs.Bool("nodenoise", false, "start with RNNoise off")
	nogate := fs.Bool("nogate", false, "start with the noise gate off")
	noecho := fs.Bool("noecho", false, "start with echo cancellation off")
	noagc := fs.Bool("noagc", false, "start with automatic gain control off")
	mic := fs.String("mic", set.Mic, "microphone name or prefix (default: system default)")
	out := fs.String("out", set.Out, "speaker name or prefix (default: system default)")
	fs.Usage = usage
	fs.Parse(os.Args[2:])

	ctl := &controls{}
	ctl.bitrate.Store(int32(set.Bitrate))
	ctl.denoise.Store(set.Denoise && !*nodenoise)
	ctl.gate.Store(set.Gate && !*nogate)
	ctl.aec.Store(set.Echo && !*noecho)
	ctl.agc.Store(set.AGC && !*noagc)
	ctl.ptt.Store(set.PTT)

	var l link
	relay := false
	switch os.Args[1] {
	case "listen":
		if fs.NArg() != 0 {
			usage()
		}
		l = loadOrCreateLink(*rotate)
	case "join", "relay":
		if fs.NArg() != 1 {
			usage()
		}
		var err error
		if l, err = parseLink(fs.Arg(0)); err != nil {
			log.Fatal(err)
		}
		relay = os.Args[1] == "relay"
		if !relay {
			*port = 0 // a caller takes any free port; the relay keeps the one it was given
		}
	default:
		usage()
	}
	n := newNode(l, *name, ctl, set)

	logFile, logPath := openLog()
	openStats()
	stat("start", map[string]any{"ver": version, "name": *name, "os": runtime.GOOS + "/" + runtime.GOARCH, "mode": os.Args[1]})
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	log.Println("yap", version, "proto", proto, os.Args[1:])
	host, _ := os.Hostname()
	log.Printf("host %s · %s/%s · %s · %d cpu · name %q · config %s",
		host, runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.NumCPU(), *name, filepath.Dir(set.path))
	log.Printf("settings: bitrate %d · denoise %v · gate %v · echo %v · mic %q · out %q · %d remembered volumes",
		set.Bitrate, ctl.denoise.Load(), ctl.gate.Load(), ctl.aec.Load(), set.Mic, set.Out, len(set.Volumes))

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	n.conn = conn
	log.Println("link", n.link)

	if relay { // daemon: no audio, no TUI, just forward for everyone
		log.Printf("relay on udp %d — forwarding for anyone on this link", *port)
		n.runRelay()
		return
	}

	if *mic != set.Mic || *out != set.Out {
		set.Mic, set.Out = *mic, *out
		set.save()
	}
	n.audio, err = openAudio(*mic, *out)
	if err != nil {
		log.Fatal("audio:", err)
	}
	defer n.audio.Close()
	n.audio.aecOn.Store(ctl.aec.Load())
	n.audio.setAEC(set.AECSuppress, set.AECSuppressActive)
	n.web, n.logs = &webServer{n: n}, &logRing{}
	log.SetOutput(io.MultiWriter(os.Stderr, logFile, n.logs))
	if set.Web {
		n.web.start()
	}
	log.Println("audio:", n.audio.describe())
	go func() { // never blocks startup; the TUI asks, the log keeps it
		if tag := checkUpdate(); tag != "" {
			n.update.Store(&tag)
			log.Printf("update available: %s (you run %s) — yap update", tag, version)
		}
	}()
	log.Printf("buffers: jitter %d–%d frames (%d–%d ms) · playback %d frames · peer timeout %s",
		minPrebuf, maxPrebuf, minPrebuf*20, maxPrebuf*20, playTarget, peerTimeout)

	if *plain {
		fmt.Printf("\n  %s\n\n", n.link)
		n.run()
		return
	}
	notice := ""
	if os.Args[1] == "listen" { // the host's link is what friends need: hand it over right away
		copyToClipboard(n.link.String())
		notice = "your link is in the clipboard — send it to friends, they run: yap join <link>"
	}
	ui := newUI(n, logPath, notice)
	log.SetOutput(io.MultiWriter(logFile, n.logs, ui))
	go n.run()
	wantUpdate, err := ui.Run()
	if err != nil {
		log.Fatal(err)
	}
	if wantUpdate { // chosen in the TUI: swap the binary and come back into the same room
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
		n.audio.Close()
		if err := selfUpdate(); err != nil {
			log.Fatal("update: ", err)
		}
		if err := restart(); err != nil {
			log.Fatal("restart: ", err)
		}
	}
}

func defaultName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	h, _ := os.Hostname()
	return h
}

// openLog appends to <config>/yap/yap.log, starting over once it grows past 5 MB.
func openLog() (*os.File, string) {
	path := filepath.Join(configDir(), "yap.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		panic(err)
	}
	return f, path
}
