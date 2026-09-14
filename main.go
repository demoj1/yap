package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	answerTimeout = 60 * time.Second
	punchTimeout  = 20 * time.Second
)

func usage() {
	fmt.Fprintf(os.Stderr, `yap — one-to-one voice call, nothing else.

  yap listen [-p 0] [-nodenoise]     print a link, wait for a friend
  yap join <link> [-nodenoise]       call the friend

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
	port := fs.Int("p", 0, "UDP port (default: random)")
	nodenoise := fs.Bool("nodenoise", false, "disable RNNoise")
	fs.Parse(args)

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	l := newLink()
	fmt.Printf("\n  %s\n\n", l)
	log.Println("you are at", candidates(conn))
	log.Println("waiting for a friend...")

	offers, err := newRoom(l).listen(context.Background(), 1)
	if err != nil {
		log.Fatal("rendezvous:", err)
	}
	offer := <-offers
	log.Println("friend is at", offer.Addrs)
	if err := newRoom(l).say(hello{Role: 0, Addrs: candidates(conn)}); err != nil {
		log.Fatal("rendezvous:", err)
	}
	call(newSession(conn, l.mediaKey(), 0, !*nodenoise), conn, offer.Addrs)
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
	r := newRoom(l)
	answers, err := r.listen(context.Background(), 0)
	if err != nil {
		log.Fatal("rendezvous:", err)
	}
	cands := candidates(conn)
	log.Println("you are at", cands)
	if err := r.say(hello{Role: 1, Addrs: cands}); err != nil {
		log.Fatal("rendezvous:", err)
	}
	log.Println("calling...")
	var answer hello
	select {
	case answer = <-answers:
	case <-time.After(answerTimeout):
		log.Fatal("friend did not answer")
	}
	log.Println("friend is at", answer.Addrs)
	call(newSession(conn, l.mediaKey(), 1, !*nodenoise), conn, answer.Addrs)
}

func call(s *session, conn *net.UDPConn, peerAddrs []string) {
	a, err := openAudio()
	if err != nil {
		log.Fatal("audio:", err)
	}
	defer a.Close()
	s.run(a)
	if err := s.punch(peerAddrs, punchTimeout); err != nil {
		log.Fatal(err)
	}
	log.Println("connected:", s.peer.Load())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	stats := time.NewTicker(5 * time.Second)
	for {
		select {
		case <-stats.C:
			log.Printf("tx %d %.1f kB/s  rx %d  jitter %.1f ms | jb depth %d lost %d late %d skip %d rebuf %d | period %d underrun %d capdrop %d | mic %.0f dBFS spk %.0f dBFS",
				s.tx.Load(), float64(s.txBytes.Swap(0))/5000, s.rx.Load(), float64(s.jitUS.Load())/1000,
				s.jb.depth(), s.jb.lost.Load(), s.jb.late.Load(), s.jb.skip.Load(), s.jb.rebuf.Load(),
				a.period.Load(), a.play.underrun.Load(), a.capDrop.Load(),
				a.micPeak.take(), a.spkPeak.take())
		case <-sig:
			conn.Close()
			return
		}
	}
}
