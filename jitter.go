package main

import (
	"sync"
	"sync/atomic"
)

const (
	prebuf      = 3   // frames to collect before playout starts (60 ms)
	maxDepth    = 10  // frames; beyond that we skip ahead to cut latency
	shrinkAfter = 100 // pulls (2 s) with excess queued before one frame is dropped to win back 20 ms
)

// jitter reorders incoming Opus packets by sequence number.
type jitter struct {
	mu      sync.Mutex
	pkts    map[uint64][]byte
	next    uint64
	started bool
	starve  int // consecutive pulls with an empty buffer; a few get PLC, more means rebuffer
	excess  int // consecutive pulls that left more than prebuf queued; long runs mean latency crept up

	lost, late, skip, rebuf, stall atomic.Uint64
}

func newJitter() *jitter {
	return &jitter{pkts: map[uint64][]byte{}}
}

func (j *jitter) push(seq uint64, pkt []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.started && seq < j.next {
		j.late.Add(1)
		return
	}
	j.pkts[seq] = pkt
	if !j.started && len(j.pkts) >= prebuf {
		j.started = true
		j.next = j.minSeq()
	}
	if len(j.pkts) > maxDepth {
		j.skip.Add(1)
		j.next = j.maxSeq() - prebuf
		for s := range j.pkts {
			if s < j.next {
				delete(j.pkts, s)
			}
		}
	}
}

// pull returns the next packet in order. lost=true means the caller should
// run PLC: either a real gap (next advances) or the sender is late (stall:
// next stays, so the frame is played when it arrives instead of dropped).
// ok=false means (re)buffering.
func (j *jitter) pull() (pkt []byte, lost, ok bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.started {
		return nil, false, false
	}
	if p, has := j.pkts[j.next]; has {
		j.starve = 0
		delete(j.pkts, j.next)
		j.next++
		if len(j.pkts) > prebuf {
			j.excess++
		} else {
			j.excess = 0
		}
		if j.excess > shrinkAfter {
			j.excess = 0
			j.skip.Add(1)
			delete(j.pkts, j.next)
			j.next++
		}
		return p, false, true
	}
	if len(j.pkts) == 0 {
		j.starve++
		if j.starve > prebuf {
			j.started = false
			j.starve = 0
			j.rebuf.Add(1)
			return nil, false, false
		}
		j.stall.Add(1)
		return nil, true, true
	}
	j.next++
	j.lost.Add(1)
	return nil, true, true
}

func (j *jitter) depth() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.pkts)
}

func (j *jitter) minSeq() uint64 {
	m := ^uint64(0)
	for s := range j.pkts {
		m = min(m, s)
	}
	return m
}

func (j *jitter) maxSeq() uint64 {
	var m uint64
	for s := range j.pkts {
		m = max(m, s)
	}
	return m
}
