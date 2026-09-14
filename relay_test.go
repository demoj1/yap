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
		p.addr.Store(other.conn.LocalAddr().(*net.UDPAddr))
	}
	n.mu.Lock()
	n.peers[string(p.id)] = p
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
	audio := append([]byte{typAudio}, enc.encode(sine(8000, 440))...)
	for i := 0; i < 5; i++ {
		a.sendTo(aB, audio)
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
		b.sendTo(bA, audio)
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
