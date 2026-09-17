package main

import (
	"path/filepath"
	"testing"
)

// A group packet from src, passed on by relay q, lands in src's jitter
// buffer as if it came straight from src.
func TestFanReceiveOpensGroupPacket(t *testing.T) {
	set := &settings{path: filepath.Join(t.TempDir(), "settings.json"), Volumes: map[string]int{}}
	me := newNode(newLink(), "me", &controls{}, set)
	them := newNode(me.link, "them", &controls{}, set)
	relay := newNode(me.link, "relay", &controls{}, set)
	src := newPeer(me.link, me.id, me.nonce, them.helloOf())
	via := newPeer(me.link, me.id, me.nonce, relay.helloOf())
	me.mu.Lock()
	me.peers[string(src.id)], me.peers[string(via.id)] = src, via
	me.mu.Unlock()

	enc := newEncoder()
	opus := append([]byte(nil), enc.encode(sine(8000, 440))...)
	// Their state packet brings the group key; then a relayed group packet with audio.
	me.deliver(src, zeroAddr, append([]byte{typState, 0, stateHears}, them.group.key[:]...))
	if src.group.Load() == nil {
		t.Fatal("group key not learned from the state packet")
	}
	for i := 0; i < 5; i++ {
		sealed := them.group.seal(uint64(i), audioPayload(uint32(i), opus))
		me.fanReceive(via, append(append([]byte{typRelayedG}, src.id...), sealed...))
	}
	if d := src.jb.depth(); d != 5 {
		t.Fatalf("jitter buffer has %d frames, want 5", d)
	}
	if v := src.via.Load(); v != via {
		t.Fatal("the relay must become the way back to them")
	}
	// A packet sealed with a different key never gets in.
	other := newGroupKey(randBytes(32))
	me.fanReceive(via, append(append([]byte{typRelayedG}, src.id...), other.seal(9, audioPayload(9, opus))...))
	if d := src.jb.depth(); d != 5 {
		t.Fatalf("a foreign key got a frame in: depth %d", d)
	}
}

// Only builds new enough take part in a fan-out.
func TestFansByVersion(t *testing.T) {
	for ver, want := range map[string]bool{"": false, "v0.9.5": false, fanSince: false, "v0.9.13": true, "v1.0.0": true, "dev": true} {
		if got := (&peer{ver: ver}).fans(); got != want {
			t.Errorf("%q: fans=%v, want %v", ver, got, want)
		}
	}
}
