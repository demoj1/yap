package main

import (
	"sync"
	"sync/atomic"
)

const (
	prebuf   = 3  // frames to collect before playout starts (60 ms)
	maxDepth = 10 // frames; beyond that we skip ahead to cut latency
)

// jitter reorders incoming Opus packets by sequence number.
type jitter struct {
	mu      sync.Mutex
	pkts    map[uint64][]byte
	next    uint64
	started bool
	starve  int // consecutive pulls with an empty buffer; a few get PLC, more means rebuffer

	lost, late, skip, rebuf atomic.Uint64
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

// pull returns the next packet in order. lost=true means a gap or a sender
// running slow: the caller should run PLC. ok=false means (re)buffering.
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
