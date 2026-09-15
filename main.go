package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
)

var version = "dev" // set by -ldflags in CI

func usage() {
	fmt.Fprintf(os.Stderr, `yap — one-to-one voice call, nothing else.

  yap listen [-p 4444] [-new]     print a link, wait for a friend (link is kept across restarts)
  yap join <link>                 call the friend
  yap devices                     list microphones and speakers
  yap reset                       forget saved devices/volumes, back to defaults
  yap update                      replace this binary with the latest release

  common flags: -name <shown to the friend>  -mic <name>  -out <name>
                -plain (logs instead of the TUI)  -nodenoise
`)
	os.Exit(2)
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
	}
	if os.Args[1] == "devices" {
		printDevices()
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
	aecOn := fs.Bool("aec", false, "enable acoustic echo cancellation (for speakers)")
	noagc := fs.Bool("noagc", false, "start with automatic gain control off")
	mic := fs.String("mic", set.Mic, "microphone name or prefix (default: system default)")
	out := fs.String("out", set.Out, "speaker name or prefix (default: system default)")
	fs.Usage = usage
	fs.Parse(os.Args[2:])

	ctl := &controls{}
	ctl.bitrate.Store(int32(set.Bitrate))
	ctl.denoise.Store(set.Denoise && !*nodenoise)
	ctl.gate.Store(set.Gate && !*nogate)
	ctl.aec.Store(set.AEC || *aecOn)
	ctl.agc.Store(set.AGC && !*noagc)

	var l link
	switch os.Args[1] {
	case "listen":
		if fs.NArg() != 0 {
			usage()
		}
		l = loadOrCreateLink(*rotate)
	case "join":
		if fs.NArg() != 1 {
			usage()
		}
		var err error
		if l, err = parseLink(fs.Arg(0)); err != nil {
			log.Fatal(err)
		}
		*port = 0
	default:
		usage()
	}
	n := newNode(l, *name, ctl, set)

	logFile, logPath := openLog()
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	log.Println("yap", version, "proto", proto, os.Args[1:])
	host, _ := os.Hostname()
	log.Printf("host %s · %s/%s · %s · %d cpu · name %q · config %s",
		host, runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.NumCPU(), *name, filepath.Dir(set.path))
	log.Printf("settings: bitrate %d · denoise %v · gate %v · mic %q · out %q · %d remembered volumes",
		set.Bitrate, ctl.denoise.Load(), ctl.gate.Load(), set.Mic, set.Out, len(set.Volumes))
	_ = ctl.agc.Load()
	if ctl.aec.Load() {
		log.Println("echo cancellation: on")
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	n.conn = conn
	log.Println("link", n.link)
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
	log.Println("audio:", n.audio.describe())
	go func() { // never blocks startup; the TUI shows it and the log keeps it
		if hint := checkUpdate(); hint != "" {
			n.update.Store(&hint)
			log.Println(hint)
		}
	}()
	log.Printf("buffers: jitter %d–%d frames (%d–%d ms) · playback %d frames · peer timeout %s",
		minPrebuf, maxPrebuf, minPrebuf*20, maxPrebuf*20, playTarget, peerTimeout)

	if *plain {
		fmt.Printf("\n  %s\n\n", n.link)
		n.run()
		return
	}
	ui := newUI(n, logPath)
	log.SetOutput(io.MultiWriter(logFile, ui))
	go n.run()
	if err := ui.Run(); err != nil {
		log.Fatal(err)
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
	dir, err := os.UserConfigDir()
	if err != nil {
		panic(err)
	}
	path := filepath.Join(dir, "yap", "yap.log")
	must(os.MkdirAll(filepath.Dir(path), 0o700))
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
