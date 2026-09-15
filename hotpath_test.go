package main

import "testing"

func benchNode() *node {
	l := newLink()
	n := newNode(l, "me", &controls{}, &settings{Volumes: map[string]int{}})
	for i := 0; i < 4; i++ {
		p := newPeer(l, n.id, n.nonce, hello{ID: randBytes(8), Name: "p", Nonce: randBytes(16)})
		n.peers[string(p.id)] = p
	}
	n.rebuildRoster()
	return n
}

func BenchmarkPeerList(b *testing.B) {
	n := benchNode()
	b.ReportAllocs()
	b.ResetTimer()
	var c int
	for i := 0; i < b.N; i++ {
		c += len(n.peerList())
	}
	_ = c
}

func BenchmarkAppendAudio(b *testing.B) {
	opus := make([]byte, 160)
	var buf []byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = appendAudio(buf, uint32(i), opus)
	}
	_ = buf
}
