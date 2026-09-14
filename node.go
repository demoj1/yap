package main

import (
	"context"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/demoj1/yap/internal/rnnoise"
)

const (
	answerTimeout = 20 * time.Second
	punchTimeout  = 20 * time.Second
	retryPause    = 3 * time.Second
)

// view is what a call reports to: the TUI or plain logs.
type view interface {
	state(st string, peer string)
	session(s *session)
}

// node owns the socket and the audio device for the whole run and dials
// call after call: a finished or failed call just brings it back to the
// rendezvous.
type node struct {
	conn  *net.UDPConn
	audio *audio
	link  link
	name  string
	ctl   *controls
	view  view
	cur   atomic.Pointer[session] // the call in progress, if any
}

// sendLoop runs for the life of the node so microphone frames are always
// drained; they are encoded and sent only while a call has a peer.
func (n *node) sendLoop() {
	enc := newEncoder()
	dn := rnnoise.New()
	defer dn.Close()
	bitrate := 0
	for f := range n.audio.frames {
		if n.ctl.muted.Load() {
			clear(f)
		} else if n.ctl.denoise.Load() {
			dn.Process(f[:rnnoise.FrameSize])
			dn.Process(f[rnnoise.FrameSize:])
		}
		n.audio.micPeak.observe(f)
		s := n.cur.Load()
		if s == nil {
			continue
		}
		peer := s.peer.Load()
		if peer == nil {
			continue
		}
		if b := int(n.ctl.bitrate.Load()); b != bitrate {
			must(enc.enc.SetBitrate(b * 1000))
			bitrate = b
		}
		s.send(enc.encode(f), peer)
		s.tx.Add(1)
	}
}

func (n *node) listenForever() {
	room := newRoom(n.link)
	for {
		n.view.state("waiting", "")
		ctx, cancel := context.WithCancel(context.Background())
		offers, err := room.listen(ctx, 1)
		if err != nil {
			cancel()
			log.Println("rendezvous:", err)
			time.Sleep(retryPause)
			continue
		}
		offer := <-offers
		cancel()
		if len(offer.Nonce) == 0 {
			log.Println("a friend with an old yap tried to call — ask them to update")
			continue
		}
		log.Println(offer.Name, "is at", offer.Addrs)
		mine := hello{Role: 0, Name: n.name, Nonce: randBytes(16), Addrs: candidates(n.conn)}
		if err := room.say(mine); err != nil {
			log.Println("rendezvous:", err)
			continue
		}
		n.call(n.link.mediaKey(offer.Nonce, mine.Nonce), 0, offer)
	}
}

func (n *node) joinForever() {
	room := newRoom(n.link)
	for {
		n.view.state("calling", "")
		ctx, cancel := context.WithCancel(context.Background())
		answers, err := room.listen(ctx, 0)
		if err != nil {
			cancel()
			log.Println("rendezvous:", err)
			time.Sleep(retryPause)
			continue
		}
		mine := hello{Role: 1, Name: n.name, Nonce: randBytes(16), Addrs: candidates(n.conn)}
		if err := room.say(mine); err != nil {
			cancel()
			log.Println("rendezvous:", err)
			time.Sleep(retryPause)
			continue
		}
		var answer hello
		select {
		case answer = <-answers:
		case <-time.After(answerTimeout):
			cancel()
			log.Println("no answer, retrying")
			continue
		}
		cancel()
		if len(answer.Nonce) == 0 {
			log.Println("the friend runs an old yap — ask them to update")
			time.Sleep(retryPause)
			continue
		}
		log.Println(answer.Name, "is at", answer.Addrs)
		n.call(n.link.mediaKey(mine.Nonce, answer.Nonce), 1, answer)
	}
}

func (n *node) call(key [32]byte, dir uint32, peer hello) {
	s := newSession(n.conn, key, dir, n.ctl)
	defer s.close()
	s.run(n.audio)
	n.cur.Store(s)
	defer n.cur.Store(nil)
	n.view.session(s)
	defer n.view.session(nil)
	n.view.state("punching", peer.Name)
	if err := s.punch(peer.Addrs, punchTimeout); err != nil {
		log.Println(err)
		return
	}
	log.Println("connected:", s.peer.Load())
	n.audio.play.underrun.Store(0)
	n.audio.capDrop.Store(0)
	n.view.state("connected", peer.Name)
	stats := time.NewTicker(5 * time.Second)
	defer stats.Stop()
	for {
		select {
		case <-stats.C:
			log.Println(s.stats(n.audio, 5*time.Second))
		case <-s.gone:
			log.Println(peer.Name, "is gone")
			return
		}
	}
}
