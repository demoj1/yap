package main

import (
	"net"
	"testing"
	"time"
)

// testNode is a node with a real localhost socket and no audio/rendezvous.
func testNode(t *testing.T, l link, name string) *node {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	n := newNode(l, name, &controls{}, &settings{Volumes: map[string]int{}})
	n.conn = conn
	return n
}

func (n *node) helloOf() hello {
	return hello{ID: n.id, Name: n.name, Nonce: n.nonce}
}

// addPeer registers other on n, optionally with its real address (direct).
func (n *node) addPeer(other *node, direct bool) *peer {
	p := newPeer(n.link, n.id, n.nonce, other.helloOf())
	p.since = time.Now()
	if direct {
		ap := other.conn.LocalAddr().(*net.UDPAddr).AddrPort()
		p.addr.Store(&ap)
	}
	n.mu.Lock()
	n.peers[string(p.id)] = p
	n.rebuildRoster()
	n.mu.Unlock()
	return p
}

// A and B cannot reach each other; both reach R. Audio from A must arrive
// at B through R, B must learn R as the way back, and R must never be able
// to decode the inner packet.
func TestRelayThroughThirdPeer(t *testing.T) {
	l := newLink()
	a, b, r := testNode(t, l, "a"), testNode(t, l, "b"), testNode(t, l, "r")

	// R sees both directly.
	r.addPeer(a, true)
	r.addPeer(b, true)
	// A knows B but has no address for it; R is its relay.
	aR := a.addPeer(r, true)
	aB := a.addPeer(b, false)
	aB.via.Store(aR)
	// B knows A (no address) and R.
	b.addPeer(r, true)
	bA := b.addPeer(a, false)

	go r.recvLoop()
	go b.recvLoop()

	enc := newEncoder()
	opus := append([]byte(nil), enc.encode(sine(8000, 440))...)
	for i := 0; i < 5; i++ {
		a.sendTo(aB, audioPayload(uint32(i), opus))
		time.Sleep(5 * time.Millisecond)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !bA.connected() || bA.jb.depth() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("B did not get A's audio via R: connected=%v depth=%d", bA.connected(), bA.jb.depth())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if bA.direct() {
		t.Fatal("B must not think it has A's direct address (that would be R's)")
	}
	if via := bA.via.Load(); via == nil || string(via.id) != string(r.id) {
		t.Fatal("B must learn R as its relay toward A")
	}

	// And back: B answers through the learned relay, A hears it.
	go a.recvLoop()
	for i := 0; i < 5; i++ {
		b.sendTo(bA, audioPayload(uint32(i), opus))
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(2 * time.Second)
	for aB.jb.depth() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("A did not get B's reply via R: depth=%d", aB.jb.depth())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRelayForPicksPeerThatReaches(t *testing.T) {
	l := newLink()
	a, b, r := testNode(t, l, "a"), testNode(t, l, "b"), testNode(t, l, "r")
	aR := a.addPeer(r, true)
	aB := a.addPeer(b, false)
	if a.relayFor(aB) != nil {
		t.Fatal("R has not claimed to reach B yet")
	}
	reach := [][]byte{b.id}
	aR.reach.Store(&reach)
	if a.relayFor(aB) != aR {
		t.Fatal("R reaches B and is direct: must be picked")
	}
}

// Forward envelopes share the pair's packet counter with audio; they must
// not show up as lost frames on the relay's own jitter buffer.
func TestRelayEnvelopesAreNotLostAudio(t *testing.T) {
	l := newLink()
	a, b, r := testNode(t, l, "a"), testNode(t, l, "b"), testNode(t, l, "r")
	rA := r.addPeer(a, true)
	r.addPeer(b, true)
	aR := a.addPeer(r, true)
	aB := a.addPeer(b, false)
	aB.via.Store(aR)
	go r.recvLoop()

	enc := newEncoder()
	opus := append([]byte(nil), enc.encode(sine(8000, 440))...)
	for i := 0; i < 20; i++ {
		p := audioPayload(uint32(i), opus)
		a.sendTo(aR, p) // audio for R itself
		a.sendTo(aB, p) // envelope through R for B — interleaved on the same counter
		time.Sleep(2 * time.Millisecond)
	}
	deadline := time.Now().Add(2 * time.Second)
	for rA.rx.Load() < 20 {
		if time.Now().After(deadline) {
			t.Fatalf("R got only %d audio frames from A", rA.rx.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		rA.nextFrame()
	}
	if lost := rA.jb.lost.Load(); lost != 0 {
		t.Fatalf("relay envelopes were counted as %d lost audio frames", lost)
	}
}
