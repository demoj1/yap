package main

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/stun/v3"
	"golang.org/x/crypto/chacha20poly1305"
)

var stunServers = []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302", "stun.sipgate.net:3478"}

const peerTimeout = 10 * time.Second

var bitrates = []int{12, 16, 24, 32, 48, 64, 96, 128, 160} // kbps

// controls are the live knobs the UI turns; loops read them every frame.
type controls struct {
	bitrate atomic.Int32 // kbps
	muted   atomic.Bool
	denoise atomic.Bool
	volume  atomic.Int32 // percent applied to the friend's audio
}

func (c *controls) stepBitrate(dir int) {
	cur := int(c.bitrate.Load())
	for i, b := range bitrates {
		if b == cur {
			c.bitrate.Store(int32(bitrates[max(0, min(len(bitrates)-1, i+dir))]))
			return
		}
	}
}

// Packet: [seq uint64][AEAD(nonce = dir(4) || seq(8), aad = seq)].
// dir is 0 for the listener's stream and 1 for the joiner's, so both sides
// can share one key without nonce reuse. A session is one call; it ends
// when the friend stops sending (gone) or we hang up (close).
type session struct {
	conn      *net.UDPConn
	aead      cipher.AEAD
	dir       uint32
	ctl       *controls
	peer      atomic.Pointer[net.UDPAddr]
	seq       atomic.Uint64
	rx, tx    atomic.Uint64
	txBytes   atomic.Uint64
	jitUS     atomic.Int64 // RFC 3550 style interarrival jitter, microseconds
	lastRx    atomic.Int64 // unix nanos
	jb        *jitter
	lastStats atomic.Pointer[string]
	ready     chan struct{} // closed on the first authenticated packet from the friend
	gone      chan struct{} // closed when the friend has been silent for peerTimeout
	done      chan struct{} // closed by close()
	once      sync.Once
	goneOnc   sync.Once
	doneOnc   sync.Once
	recvEnd   chan struct{} // closed when recvLoop has released the socket
}

func newSession(conn *net.UDPConn, key [32]byte, dir uint32, ctl *controls) *session {
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		panic(err)
	}
	return &session{conn: conn, aead: aead, dir: dir, ctl: ctl, jb: newJitter(),
		ready: make(chan struct{}), gone: make(chan struct{}), done: make(chan struct{}), recvEnd: make(chan struct{})}
}

func (s *session) run(a *audio) {
	go s.recvLoop()
	go s.playLoop(a)
}

// close ends the call and waits for the socket to be free for STUN again.
func (s *session) close() {
	s.doneOnc.Do(func() { close(s.done) })
	<-s.recvEnd
}

func (s *session) recvLoop() {
	defer close(s.recvEnd)
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint32(nonce, 1-s.dir)
	buf := make([]byte, 1500)
	for {
		s.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := s.conn.ReadFromUDP(buf)
		select {
		case <-s.done:
			return
		default:
		}
		if err != nil {
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				return
			}
			if s.connected() && time.Since(time.Unix(0, s.lastRx.Load())) > peerTimeout {
				s.goneOnc.Do(func() { close(s.gone) })
			}
			continue
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
		now := time.Now()
		if last := s.lastRx.Swap(now.UnixNano()); last != 0 && len(plain) > 0 {
			d := (now.Sub(time.Unix(0, last)) - 20*time.Millisecond).Microseconds()
			if d < 0 {
				d = -d
			}
			j := s.jitUS.Load()
			s.jitUS.Store(j + (d-j)/16)
		}
		if len(plain) == 0 {
			continue
		}
		s.jb.push(binary.BigEndian.Uint64(buf[:8]), plain)
	}
}

func (s *session) connected() bool {
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

func (s *session) playLoop(a *audio) {
	dec := newDecoder()
	for {
		select {
		case <-a.play.need:
		case <-s.done:
			return
		}
		for a.play.len() < playTarget*frameSize {
			pkt, lost, ok := s.jb.pull()
			if !ok {
				break
			}
			var pcm []int16
			if lost {
				pcm = dec.decodeLost()
			} else {
				pcm = dec.decode(pkt)
			}
			if v := int32(s.ctl.volume.Load()); v != 100 {
				for i, x := range pcm {
					pcm[i] = int16(max(-32768, min(32767, int32(x)*v/100)))
				}
			}
			a.spkPeak.observe(pcm)
			a.play.push(pcm)
		}
	}
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

// stats reports counters plus the send rate since the previous call.
func (s *session) stats(a *audio, since time.Duration) string {
	line := fmt.Sprintf("tx %d %.1f kB/s  rx %d  jitter %.1f ms | jb depth %d lost %d stall %d late %d skip %d rebuf %d | period %d underrun %d capdrop %d",
		s.tx.Load(), float64(s.txBytes.Swap(0))/1000/since.Seconds(), s.rx.Load(), float64(s.jitUS.Load())/1000,
		s.jb.depth(), s.jb.lost.Load(), s.jb.stall.Load(), s.jb.late.Load(), s.jb.skip.Load(), s.jb.rebuf.Load(),
		a.period.Load(), a.play.underrun.Load(), a.capDrop.Load())
	s.lastStats.Store(&line)
	return line
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
