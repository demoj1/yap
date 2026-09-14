package main

import (
	"bytes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const peerTimeout = 10 * time.Second

// Payload types, first byte of a non-empty sealed payload. An empty payload
// is a ping. Relay packets carry an inner packet sealed with the pair key of
// the two ends, so the relay authenticates the envelope but cannot hear.
//
// The packet seq is the AEAD nonce and counts every packet on the pair,
// forward envelopes included; audio carries its own frame number so the
// jitter buffer never mistakes a relay envelope for a lost frame.
const (
	typAudio   = 0 // [0][frame uint32][opus]
	typForward = 1 // [1][dst id 8][inner packet]   to a relay: pass this on
	typRelayed = 2 // [2][src id 8][inner packet]   from a relay: this came from src
)

const (
	idLen     = 8
	audioHead = 1 + 4
	proto     = 2 // bumped whenever the wire format changes; hellos with another value are ignored
)

// audioPayload frames one Opus packet as a typAudio payload.
func audioPayload(frame uint32, opus []byte) []byte {
	out := make([]byte, audioHead, audioHead+len(opus))
	out[0] = typAudio
	binary.BigEndian.PutUint32(out[1:], frame)
	return append(out, opus...)
}

// parseAudio splits a typAudio payload into frame number and Opus data.
func parseAudio(plain []byte) (uint64, []byte, bool) {
	if len(plain) < audioHead || plain[0] != typAudio {
		return 0, nil, false
	}
	return uint64(binary.BigEndian.Uint32(plain[1:])), plain[audioHead:], true
}

// peer is one other participant of the mesh: its pair key, the address its
// packets come from, and its own jitter buffer + decoder so the mixer can
// pull one frame from each peer per output frame.
//
// Packet: [seq uint64][AEAD(nonce = dir(4) || seq(8), aad = seq)]. The pair
// key comes from both run nonces (sorted, so both ends agree); dir is 0 for
// the side with the smaller ID and 1 for the other, so one key serves both
// directions without nonce reuse.
type peer struct {
	id       []byte
	name     string
	nonce    []byte
	aead     cipher.AEAD
	dir      uint32 // our sending direction toward this peer
	addr     atomic.Pointer[net.UDPAddr]
	seq      atomic.Uint64
	rx, tx   atomic.Uint64
	txBytes  atomic.Uint64
	jitUS    atomic.Int64 // RFC 3550 style interarrival jitter, microseconds
	lastRx   atomic.Int64 // unix nanos
	jb       *jitter
	dec      *decoder
	level    peak
	volume   atomic.Int32             // percent, applied in the mixer
	joinedAt int64                    // roster order
	since    time.Time                // when we learned of them; never-connected peers expire from this
	punching atomic.Bool              // one punch goroutine at a time
	via      atomic.Pointer[peer]     // relay we reach this peer through when direct punching failed
	reach    atomic.Pointer[[][]byte] // IDs this peer said it talks to directly (from its hello)
	ready    chan struct{}            // closed on the first authenticated packet from them
	gone     chan struct{}            // closed when they have been silent for peerTimeout
	once     sync.Once
	goneOnc  sync.Once
}

func newPeer(l link, myID []byte, myNonce []byte, h hello) *peer {
	a, b := myNonce, h.Nonce
	if bytes.Compare(a, b) > 0 {
		a, b = b, a
	}
	key := l.mediaKey(a, b)
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		panic(err)
	}
	var dir uint32
	if bytes.Compare(myID, h.ID) > 0 {
		dir = 1
	}
	return &peer{id: h.ID, name: h.Name, nonce: h.Nonce, aead: aead, dir: dir,
		jb: newJitter(), dec: newDecoder(),
		ready: make(chan struct{}), gone: make(chan struct{})}
}

// seal builds one packet toward this peer. An empty payload is a ping: it
// authenticates us and opens the NAT but carries no audio.
func (p *peer) seal(payload []byte) []byte {
	seq := p.seq.Add(1) - 1
	buf := make([]byte, 8, 8+len(payload)+p.aead.Overhead())
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint32(nonce, p.dir)
	binary.BigEndian.PutUint64(buf, seq)
	binary.BigEndian.PutUint64(nonce[4:], seq)
	return p.aead.Seal(buf, nonce, payload, buf[:8])
}

// open authenticates a packet as coming from this peer and returns its
// payload (nil, true for a ping). It does not touch counters.
func (p *peer) open(pkt []byte) ([]byte, bool) {
	if len(pkt) < 8+p.aead.Overhead() {
		return nil, false
	}
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint32(nonce, 1-p.dir)
	copy(nonce[4:], pkt[:8])
	plain, err := p.aead.Open(nil, nonce, pkt[8:], pkt[:8])
	if err != nil {
		return nil, false
	}
	return plain, true
}

// accept records an authenticated packet: locks the peer to the address it
// came from (nil for a relayed packet — the relay's address is not theirs),
// updates liveness/jitter, and queues audio.
func (p *peer) accept(from *net.UDPAddr, seq uint64, audio []byte) {
	if from != nil {
		p.addr.Store(from)
	}
	p.once.Do(func() { close(p.ready) })
	if audio != nil {
		p.rx.Add(1) // frames, not envelopes: keeps rx comparable with the sender's tx
	}
	now := time.Now()
	if last := p.lastRx.Swap(now.UnixNano()); last != 0 && audio != nil {
		d := (now.Sub(time.Unix(0, last)) - 20*time.Millisecond).Microseconds()
		if d < 0 {
			d = -d
		}
		j := p.jitUS.Load()
		p.jitUS.Store(j + (d-j)/16)
	}
	if audio != nil {
		p.jb.push(seq, audio)
	}
}

// direct reports whether we have this peer's own address.
func (p *peer) direct() bool { return p.addr.Load() != nil }

// reaches reports whether the peer claimed a direct link to id in its hello.
func (p *peer) reaches(id []byte) bool {
	r := p.reach.Load()
	if r == nil {
		return false
	}
	for _, x := range *r {
		if string(x) == string(id) {
			return true
		}
	}
	return false
}

func (p *peer) connected() bool {
	select {
	case <-p.ready:
		return true
	default:
		return false
	}
}

func (p *peer) silentFor() time.Duration {
	last := p.lastRx.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

func (p *peer) markGone() { p.goneOnc.Do(func() { close(p.gone) }) }

// quietPeak is the loudest sample a frame may have and still be dropped
// unheard to win back latency (~ -44 dBFS; RNNoise leaves silence near 0).
const quietPeak = 200

// nextFrame pulls one 20 ms frame for the mixer: decoded audio, PLC for a
// gap, or nil while (re)buffering. When the buffer has crept above its
// target (a burst of late packets), one silent frame per call is decoded and
// discarded so latency drifts back down without an audible skip.
func (p *peer) nextFrame() []int16 {
	for catchUp := true; ; catchUp = false {
		pkt, lost, ok := p.jb.pull()
		if !ok {
			return nil
		}
		var pcm []int16
		if lost {
			pcm = p.dec.decodeLost()
		} else {
			pcm = p.dec.decode(pkt)
		}
		if catchUp && !lost && p.jb.depth() > p.jb.target()+1 && isQuiet(pcm) {
			p.jb.skip.Add(1)
			continue
		}
		return pcm
	}
}

func isQuiet(pcm []int16) bool {
	for _, x := range pcm {
		if x > quietPeak || x < -quietPeak {
			return false
		}
	}
	return true
}

func (p *peer) stats(since time.Duration) string {
	return fmt.Sprintf("%s: tx %d %.1f kB/s  rx %d  jitter %.1f ms | jb %d/%d lost %d stall %d late %d skip %d rebuf %d",
		p.name, p.tx.Load(), float64(p.txBytes.Swap(0))/1000/since.Seconds(), p.rx.Load(), float64(p.jitUS.Load())/1000,
		p.jb.depth(), p.jb.target(), p.jb.lost.Load(), p.jb.stall.Load(), p.jb.late.Load(), p.jb.skip.Load(), p.jb.rebuf.Load())
}
