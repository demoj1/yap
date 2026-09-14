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
	volume   atomic.Int32  // percent, applied in the mixer
	joinedAt int64         // roster order
	punching atomic.Bool   // one punch goroutine at a time
	ready    chan struct{} // closed on the first authenticated packet from them
	gone     chan struct{} // closed when they have been silent for peerTimeout
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

// accept records an authenticated packet from addr: locks the peer to the
// address, updates liveness/jitter, and queues audio.
func (p *peer) accept(from *net.UDPAddr, seq uint64, plain []byte) {
	p.addr.Store(from)
	p.once.Do(func() { close(p.ready) })
	p.rx.Add(1)
	now := time.Now()
	if last := p.lastRx.Swap(now.UnixNano()); last != 0 && len(plain) > 0 {
		d := (now.Sub(time.Unix(0, last)) - 20*time.Millisecond).Microseconds()
		if d < 0 {
			d = -d
		}
		j := p.jitUS.Load()
		p.jitUS.Store(j + (d-j)/16)
	}
	if len(plain) > 0 {
		p.jb.push(seq, plain)
	}
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

// nextFrame pulls one 20 ms frame for the mixer: decoded audio, PLC for a
// gap, or nil while (re)buffering.
func (p *peer) nextFrame() []int16 {
	pkt, lost, ok := p.jb.pull()
	if !ok {
		return nil
	}
	if lost {
		return p.dec.decodeLost()
	}
	return p.dec.decode(pkt)
}

func (p *peer) stats(since time.Duration) string {
	return fmt.Sprintf("%s: tx %d %.1f kB/s  rx %d  jitter %.1f ms | jb %d/%d lost %d stall %d late %d skip %d rebuf %d",
		p.name, p.tx.Load(), float64(p.txBytes.Swap(0))/1000/since.Seconds(), p.rx.Load(), float64(p.jitUS.Load())/1000,
		p.jb.depth(), p.jb.target(), p.jb.lost.Load(), p.jb.stall.Load(), p.jb.late.Load(), p.jb.skip.Load(), p.jb.rebuf.Load())
}
