package main

import (
	"sync"
	"sync/atomic"
)

const (
	minPrebuf = 5  // frames of playout cushion in calm conditions (100 ms)
	maxPrebuf = 10 // ceiling the buffer grows to during a jitter burst (200 ms)
	maxDepth  = 20 // hard cap; beyond this we skip ahead so latency can't run away
	relaxRuns = 12 // clean refills before the buffer shrinks one frame toward minPrebuf
)

// jitter reorders incoming Opus packets by sequence number and adapts its
// target depth: it grows after a rebuffer (network got bursty) and slowly
// relaxes back to minPrebuf when refills are clean again, so the baseline
// latency stays low but a bad patch doesn't keep gapping.
type jitter struct {
	mu      sync.Mutex
	pkts    map[uint64][]byte
	next    uint64
	started bool
	prebuf  int // current target depth in frames
	starve  int // consecutive empty pulls; a few get PLC, more forces a rebuffer
	clean   int // consecutive in-order refills, for relaxing prebuf back down

	lost, late, skip, rebuf, stall atomic.Uint64
}

func newJitter() *jitter {
	return &jitter{pkts: map[uint64][]byte{}, prebuf: minPrebuf}
}

func (j *jitter) push(seq uint64, pkt []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.started && seq < j.next {
		j.late.Add(1)
		return
	}
	j.pkts[seq] = pkt
	if !j.started && len(j.pkts) >= j.prebuf {
		j.started = true
		j.next = j.minSeq()
	}
	if len(j.pkts) > maxDepth {
		j.skip.Add(1)
		j.next = j.maxSeq() - uint64(j.prebuf)
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
		if j.clean++; j.clean >= relaxRuns && j.prebuf > minPrebuf {
			j.prebuf--
			j.clean = 0
		}
		return p, false, true
	}
	if len(j.pkts) == 0 {
		j.starve++
		if j.starve > j.prebuf {
			j.started = false
			j.starve = 0
			j.clean = 0
			if j.prebuf < maxPrebuf { // bursty network: hold more before next playout
				j.prebuf++
			}
			j.rebuf.Add(1)
			return nil, false, false
		}
		j.stall.Add(1)
		return nil, true, true
	}
	j.next++
	j.clean = 0
	j.lost.Add(1)
	return nil, true, true
}

func (j *jitter) depth() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.pkts)
}

func (j *jitter) target() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.prebuf
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
