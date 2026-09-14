package main

import "sync"

const (
	prebuf   = 2  // frames to collect before playout starts (40 ms)
	maxDepth = 10 // frames; beyond that we skip ahead to cut latency
)

// jitter reorders incoming Opus packets by sequence number.
type jitter struct {
	mu      sync.Mutex
	pkts    map[uint64][]byte
	next    uint64
	started bool
	lost    uint64
}

func newJitter() *jitter {
	return &jitter{pkts: map[uint64][]byte{}}
}

func (j *jitter) push(seq uint64, pkt []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.started && seq < j.next {
		return
	}
	j.pkts[seq] = pkt
	if !j.started && len(j.pkts) >= prebuf {
		j.started = true
		j.next = j.minSeq()
	}
	if len(j.pkts) > maxDepth {
		j.next = j.maxSeq() - prebuf
		for s := range j.pkts {
			if s < j.next {
				delete(j.pkts, s)
			}
		}
	}
}

// pull returns the next packet in order. lost=true means a gap: the caller
// should run PLC. ok=false means nothing is buffered yet.
func (j *jitter) pull() (pkt []byte, lost, ok bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.started {
		return nil, false, false
	}
	if p, has := j.pkts[j.next]; has {
		delete(j.pkts, j.next)
		j.next++
		return p, false, true
	}
	if len(j.pkts) == 0 {
		j.started = false
		return nil, false, false
	}
	j.next++
	j.lost++
	return nil, true, true
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
