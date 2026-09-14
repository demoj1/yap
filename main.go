package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func usage() {
	fmt.Fprintf(os.Stderr, `yap — one-to-one voice call, nothing else.

  yap listen [-p 4444] [-host 1.2.3.4] [-nodenoise]   print a link, wait for a friend
  yap join <link> [-nodenoise]                          call the friend

`)
	os.Exit(2)
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "listen":
		listen(os.Args[2:])
	case "join":
		join(os.Args[2:])
	default:
		usage()
	}
}

func listen(args []string) {
	fs := flag.NewFlagSet("listen", flag.ExitOnError)
	port := fs.Int("p", 4444, "UDP port to listen on")
	host := fs.String("host", "", "public address to put in the link (default: ask STUN)")
	nodenoise := fs.Bool("nodenoise", false, "disable RNNoise")
	fs.Parse(args)

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	pubPort := *port
	if *host == "" {
		pub, err := publicAddr(conn)
		if err != nil {
			log.Fatalf("stun: %v (pass -host explicitly)", err)
		}
		*host, pubPort = pub.IP.String(), pub.Port
	}
	secret := newSecret()
	fmt.Printf("\n  %s\n\n", formatLink(*host, pubPort, secret))
	log.Println("waiting for a friend...")

	s := newSession(conn, keyFromSecret(secret), 0, !*nodenoise)
	call(s, conn)
}

func join(args []string) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	nodenoise := fs.Bool("nodenoise", false, "disable RNNoise")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	l, err := parseLink(fs.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		log.Fatal(err)
	}
	s := newSession(conn, l.key, 1, !*nodenoise)
	s.peer.Store(l.addr)
	log.Println("calling", l.addr)
	call(s, conn)
}

func call(s *session, conn *net.UDPConn) {
	a, err := openAudio()
	if err != nil {
		log.Fatal("audio:", err)
	}
	defer a.Close()
	s.run(a)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	stats := time.NewTicker(5 * time.Second)
	for {
		select {
		case <-s.ready:
			log.Println("connected:", s.peer.Load())
			s.ready = nil
		case <-stats.C:
			if s.rx.Load() > 0 {
				log.Printf("tx %d  rx %d  lost %d", s.tx.Load(), s.rx.Load(), s.jb.lost)
			}
		case <-sig:
			conn.Close()
			return
		}
	}
}
