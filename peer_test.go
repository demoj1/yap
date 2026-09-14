package main

import "testing"

func testPeer() *peer {
	l := newLink()
	return newPeer(l, randBytes(8), randBytes(16), hello{ID: randBytes(8), Name: "t", Nonce: randBytes(16)})
}

// A backlog of silent frames is drained one extra frame per pull until the
// buffer is back near its target; loud frames are never dropped.
func TestCatchUpDropsOnlySilence(t *testing.T) {
	p := testPeer()
	enc := newEncoder()
	silence := make([]int16, frameSize)
	loud := sine(20000, 300)
	for seq := 0; seq < 12; seq++ {
		p.jb.push(uint64(seq), append([]byte(nil), enc.encode(silence)...))
	}
	before := p.jb.depth()
	p.nextFrame()
	if d := p.jb.depth(); d != before-2 {
		t.Fatalf("silent backlog: expected two frames consumed, depth %d -> %d", before, d)
	}
	if p.jb.skip.Load() != 1 {
		t.Fatalf("one skip expected, got %d", p.jb.skip.Load())
	}

	p2 := testPeer()
	for seq := 0; seq < 12; seq++ {
		p2.jb.push(uint64(seq), append([]byte(nil), enc.encode(loud)...))
	}
	before = p2.jb.depth()
	p2.nextFrame()
	if d := p2.jb.depth(); d != before-1 {
		t.Fatalf("loud backlog must not be skipped, depth %d -> %d", before, d)
	}
	if p2.jb.skip.Load() != 0 {
		t.Fatal("loud frames were skipped")
	}
}

func TestNoCatchUpNearTarget(t *testing.T) {
	p := testPeer()
	enc := newEncoder()
	silence := make([]int16, frameSize)
	for seq := 0; seq < minPrebuf+1; seq++ {
		p.jb.push(uint64(seq), append([]byte(nil), enc.encode(silence)...))
	}
	p.nextFrame()
	if p.jb.skip.Load() != 0 {
		t.Fatal("must not skip when depth is within target+1")
	}
}
