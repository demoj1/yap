package main

import (
	"crypto/cipher"
	"encoding/binary"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"
)

// Fan-out through a relay. Audio and video for several people behind the
// same relay go up once, sealed with the sender's group key (handed to
// everyone in the state packet), and the relay copies the packet to each
// of them. The relay still cannot read it. Older relays and clients get
// the one-packet-per-person path as before.
//
// [9][n u8][dst id 8 × n][seq u64][group-sealed payload]   sender → relay
// [10][src id 8][seq u64][group-sealed payload]            relay → each dst

const fanSince = "v0.9.12" // builds newer than this understand typFan / typRelayedG

type groupKey struct {
	key  [chacha20poly1305.KeySize]byte
	aead cipher.AEAD
}

func newGroupKey(key []byte) *groupKey {
	g := &groupKey{}
	copy(g.key[:], key)
	aead, err := chacha20poly1305.New(g.key[:])
	if err != nil {
		panic(err)
	}
	g.aead = aead
	return g
}

// seal wraps payload for everyone holding the key: [seq][ciphertext].
func (g *groupKey) seal(seq uint64, payload []byte) []byte {
	buf := make([]byte, 8, 8+len(payload)+g.aead.Overhead())
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(buf, seq)
	binary.BigEndian.PutUint64(nonce[4:], seq)
	return g.aead.Seal(buf, nonce, payload, buf[:8])
}

func (g *groupKey) open(pkt []byte) ([]byte, bool) {
	if len(pkt) < 8+g.aead.Overhead() {
		return nil, false
	}
	nonce := make([]byte, chacha20poly1305.NonceSize)
	copy(nonce[4:], pkt[:8])
	plain, err := g.aead.Open(nil, nonce, pkt[8:], pkt[:8])
	return plain, err == nil
}

// fans reports whether p's build can take part in a fan-out.
func (p *peer) fans() bool { return p.ver == "dev" || newerThan(p.ver, fanSince) }

// fanOut sends payload to every peer in the list: directly where it can,
// and once per relay for the people behind it.
func (n *node) fanOut(peers []*peer, payload []byte) {
	var relays map[*peer][]*peer
	for _, p := range peers {
		if !p.connected() {
			continue
		}
		via := p.via.Load()
		if p.direct() || via == nil || !via.direct() || !via.fans() || !p.fans() || p.group.Load() == nil {
			n.sendTo(p, payload) // the old way: they cannot open a group packet
			continue
		}
		if relays == nil {
			relays = map[*peer][]*peer{}
		}
		relays[via] = append(relays[via], p)
	}
	if relays == nil {
		return
	}
	sealed := n.group.seal(n.groupSeq.Add(1), payload)
	for via, dsts := range relays {
		pkt := make([]byte, 0, 2+idLen*len(dsts)+len(sealed))
		pkt = append(pkt, typFan, byte(len(dsts)))
		for _, p := range dsts {
			pkt = append(pkt, p.id...)
			p.tx.Add(1)
			p.txBytes.Add(uint64(len(payload)))
		}
		n.sendTo(via, append(pkt, sealed...))
	}
}

// fanForward is the relay's part: copy the group packet to each listed
// peer we have a direct address for.
func (n *node) fanForward(q *peer, plain []byte) {
	if len(plain) < 2 {
		return
	}
	cnt := int(plain[1])
	if len(plain) < 2+cnt*idLen {
		return
	}
	body := plain[2+cnt*idLen:]
	for i := 0; i < cnt; i++ {
		dst := n.peerByID(plain[2+i*idLen : 2+(i+1)*idLen])
		if dst == nil || !dst.direct() {
			n.dropped(q, "fan", dst)
			continue
		}
		out := make([]byte, 0, 1+idLen+len(body))
		out = append(out, typRelayedG)
		out = append(out, q.id...)
		out = append(out, body...)
		n.conn.WriteToUDPAddrPort(dst.seal(out), *dst.addr.Load())
	}
}

// fanReceive unwraps a group packet a relay passed on from src.
func (n *node) fanReceive(q *peer, plain []byte) {
	if len(plain) < 1+idLen+8 {
		return
	}
	src := n.peerByID(plain[1 : 1+idLen])
	if src == nil {
		n.dropped(q, "fan-relayed", nil)
		return
	}
	g := src.group.Load()
	if g == nil {
		return
	}
	inner, ok := g.open(plain[1+idLen:])
	if !ok {
		n.staleKey(src)
		return
	}
	if !src.direct() && src.via.Load() == nil {
		src.via.Store(q)
	}
	n.deliver(src, zeroAddr, inner)
}

// learnGroup keeps p's group key from its state packet.
func (p *peer) learnGroup(key []byte) {
	if g := p.group.Load(); g != nil && string(g.key[:]) == string(key) {
		return
	}
	p.group.Store(newGroupKey(key))
}

var _ = atomic.Pointer[groupKey]{}
