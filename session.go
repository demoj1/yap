package main

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/demoj1/yap/internal/rnnoise"
	"github.com/pion/stun/v3"
	"golang.org/x/crypto/chacha20poly1305"
)

var stunServers = []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302", "stun.sipgate.net:3478"}

// Packet: [seq uint64][AEAD(nonce = dir(4) || seq(8), aad = seq)].
// dir is 0 for the listener's stream and 1 for the joiner's, so both sides
// can share one key without nonce reuse.
type session struct {
	conn    *net.UDPConn
	aead    cipher.AEAD
	dir     uint32
	peer    atomic.Pointer[net.UDPAddr]
	seq     atomic.Uint64
	rx, tx  atomic.Uint64
	txBytes atomic.Uint64
	jitUS   atomic.Int64 // RFC 3550 style interarrival jitter, microseconds
	lastRx  time.Time
	jb      *jitter
	denoise bool
	once    sync.Once
	ready   chan struct{}
}

func newSession(conn *net.UDPConn, key [32]byte, dir uint32, denoise bool) *session {
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		panic(err)
	}
	return &session{conn: conn, aead: aead, dir: dir, jb: newJitter(), denoise: denoise, ready: make(chan struct{})}
}

func (s *session) run(a *audio) {
	go s.sendLoop(a)
	go s.recvLoop()
	go s.playLoop(a)
}

func (s *session) sendLoop(a *audio) {
	enc := newEncoder()
	var dn *rnnoise.State
	if s.denoise {
		dn = rnnoise.New()
	}
	for f := range a.frames {
		peer := s.peer.Load()
		if peer == nil {
			continue
		}
		if s.denoise {
			dn.Process(f[:rnnoise.FrameSize])
			dn.Process(f[rnnoise.FrameSize:])
		}
		s.send(enc.encode(f), peer)
		s.tx.Add(1)
	}
}

func (s *session) recvLoop() {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint32(nonce, 1-s.dir)
	buf := make([]byte, 1500)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 8+s.aead.Overhead() {
			continue
		}
		copy(nonce[4:], buf[:8])
		plain, err := s.aead.Open(nil, nonce, buf[8:n], buf[:8])
		if err != nil {
			continue
		}
		s.peer.Store(from)
		s.once.Do(func() { close(s.ready) })
		s.rx.Add(1)
		if len(plain) == 0 {
			continue
		}
		now := time.Now()
		if !s.lastRx.IsZero() {
			d := (now.Sub(s.lastRx) - 20*time.Millisecond).Microseconds()
			if d < 0 {
				d = -d
			}
			j := s.jitUS.Load()
			s.jitUS.Store(j + (d-j)/16)
		}
		s.lastRx = now
		s.jb.push(binary.BigEndian.Uint64(buf[:8]), plain)
	}
}

func (s *session) playLoop(a *audio) {
	dec := newDecoder()
	for range a.play.need {
		for a.play.len() < frameSize {
			pkt, lost, ok := s.jb.pull()
			if !ok {
				break
			}
			if lost {
				a.play.push(dec.decodeLost())
			} else {
				a.play.push(dec.decode(pkt))
			}
		}
	}
}

// publicAddr asks STUN servers how this socket looks from the internet.
// Several are tried because e.g. Google is blocked on some networks.
func publicAddr(conn *net.UDPConn) (addr *net.UDPAddr, err error) {
	for _, s := range stunServers {
		if addr, err = stunQuery(conn, s); err == nil {
			return addr, nil
		}
	}
	return nil, err
}

func stunQuery(conn *net.UDPConn, server string) (*net.UDPAddr, error) {
	srv, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, err
	}
	req := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	if _, err := conn.WriteToUDP(req.Raw, srv); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	msg := &stun.Message{Raw: buf[:n]}
	if err := msg.Decode(); err != nil {
		return nil, err
	}
	var xor stun.XORMappedAddress
	if err := xor.GetFrom(msg); err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: xor.IP, Port: xor.Port}, nil
}

// send seals one payload as [seq][AEAD]. An empty payload is a ping: it
// authenticates the sender and opens the NAT but carries no audio.
func (s *session) send(payload []byte, to *net.UDPAddr) {
	seq := s.seq.Add(1) - 1
	buf := make([]byte, 8, 8+len(payload)+s.aead.Overhead())
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint32(nonce, s.dir)
	binary.BigEndian.PutUint64(buf, seq)
	binary.BigEndian.PutUint64(nonce[4:], seq)
	pkt := s.aead.Seal(buf, nonce, payload, buf[:8])
	if _, err := s.conn.WriteToUDP(pkt, to); err != nil {
		log.Println("send:", err)
	}
	s.txBytes.Add(uint64(len(pkt)))
}

// punch pings every candidate address of the peer until one of its packets
// gets through (s.ready) or we give up.
func (s *session) punch(cands []string, timeout time.Duration) error {
	var addrs []*net.UDPAddr
	for _, c := range cands {
		a, err := net.ResolveUDPAddr("udp4", c)
		if err != nil {
			panic(err)
		}
		addrs = append(addrs, a)
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, a := range addrs {
			s.send(nil, a)
		}
		select {
		case <-s.ready:
			return nil
		case <-deadline:
			return errors.New("could not punch through NAT (symmetric NAT on one side?)")
		case <-tick.C:
		}
	}
}
