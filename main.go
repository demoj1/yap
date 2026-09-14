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
)

var version = "dev" // set by -ldflags in CI

func usage() {
	fmt.Fprintf(os.Stderr, `yap — one-to-one voice call, nothing else.

  yap listen [-p 4444] [-new]     print a link, wait for a friend (link is kept across restarts)
  yap join <link>                 call the friend
  yap devices                     list microphones and speakers
  yap reset                       forget saved devices/volumes, back to defaults

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
	mic := fs.String("mic", set.Mic, "microphone name or prefix (default: system default)")
	out := fs.String("out", set.Out, "speaker name or prefix (default: system default)")
	fs.Usage = usage
	fs.Parse(os.Args[2:])

	n := &node{name: *name, ctl: &controls{}, set: set}
	n.ctl.bitrate.Store(int32(set.Bitrate))
	n.ctl.volume.Store(100)
	denoise := set.Denoise
	if *nodenoise {
		denoise = false
	}
	n.ctl.denoise.Store(denoise)

	var run func()
	switch os.Args[1] {
	case "listen":
		if fs.NArg() != 0 {
			usage()
		}
		n.link = loadOrCreateLink(*rotate)
		run = n.listenForever
	case "join":
		if fs.NArg() != 1 {
			usage()
		}
		l, err := parseLink(fs.Arg(0))
		if err != nil {
			log.Fatal(err)
		}
		n.link = l
		*port = 0
		run = n.joinForever
	default:
		usage()
	}

	logFile, logPath := openLog()
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	log.Println("yap", version, os.Args[1:])

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	n.conn = conn
	log.Println("link", n.link)
	log.Println("you are at", candidates(conn))
	if *mic != set.Mic || *out != set.Out {
		set.Mic, set.Out = *mic, *out
		set.save()
	}
	n.audio, err = openAudio(*mic, *out)
	if err != nil {
		log.Fatal("audio:", err)
	}
	defer n.audio.Close()
	go n.sendLoop()

	if *plain {
		fmt.Printf("\n  %s\n\n", n.link)
		n.view = plainView{}
		run()
		return
	}
	ui := newUI(n, logPath)
	log.SetOutput(io.MultiWriter(logFile, ui))
	n.view = ui
	go run()
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
