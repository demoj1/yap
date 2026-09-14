package main

import (
	"log"
	"time"
)

// plainView logs state changes and a stats line every 5 s.
type plainView struct {
	n    *node
	stop chan struct{}
}

func newPlainView(n *node) *plainView { return &plainView{n: n} }

func (v *plainView) state(st, peer string) { log.Println(st, peer) }

func (v *plainView) session(s *session) {
	if v.stop != nil {
		close(v.stop)
		v.stop = nil
	}
	if s == nil {
		return
	}
	v.stop = make(chan struct{})
	go func(stop chan struct{}) {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if s.connected() {
					log.Println(s.stats(v.n.audio, 5*time.Second))
				}
			case <-stop:
				return
			}
		}
	}(v.stop)
}
