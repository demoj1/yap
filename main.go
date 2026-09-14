package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/user"
)

func usage() {
	fmt.Fprintf(os.Stderr, `yap — one-to-one voice call, nothing else.

  yap listen [-p 4444] [-new]     print a link, wait for a friend (link is kept across restarts)
  yap join <link>                 call the friend

  common flags: -name <shown to the friend>  -plain (logs instead of the TUI)  -nodenoise
`)
	os.Exit(2)
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	port := fs.Int("p", 4444, "UDP port (listen only)")
	rotate := fs.Bool("new", false, "forget the saved link and make a new one (listen only)")
	name := fs.String("name", defaultName(), "your name, shown to the friend")
	plain := fs.Bool("plain", false, "plain logs instead of the TUI")
	nodenoise := fs.Bool("nodenoise", false, "start with RNNoise off")
	fs.Usage = usage
	fs.Parse(os.Args[2:])

	n := &node{name: *name, ctl: &controls{}}
	n.ctl.bitrate.Store(96)
	n.ctl.volume.Store(100)
	n.ctl.denoise.Store(!*nodenoise)

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

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	n.conn = conn
	n.audio, err = openAudio()
	if err != nil {
		log.Fatal("audio:", err)
	}
	defer n.audio.Close()
	go n.sendLoop()

	if *plain {
		fmt.Printf("\n  %s\n\n", n.link)
		log.Println("you are at", candidates(conn))
		n.view = newPlainView(n)
		run()
		return
	}
	ui := newUI(n)
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
