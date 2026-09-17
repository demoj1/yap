package main

import (
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"
)

// videoPackets cuts a frame the way videoSender does.
func videoPackets(id uint32, key bool, data []byte) [][]byte {
	chunks := (len(data) + videoChunk - 1) / videoChunk
	var out [][]byte
	for c := 0; c < chunks; c++ {
		pkt := append([]byte{typVideo}, 0, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(pkt[1:], id)
		binary.BigEndian.PutUint16(pkt[5:], uint16(c))
		binary.BigEndian.PutUint16(pkt[7:], uint16(chunks))
		if key {
			pkt[9] = videoFlagKey
		}
		out = append(out, append(pkt, data[c*videoChunk:min(len(data), (c+1)*videoChunk)]...))
	}
	return out
}

func videoNode(t *testing.T) (*node, *peer) {
	set := &settings{path: filepath.Join(t.TempDir(), "settings.json"), Volumes: map[string]int{}}
	n := newNode(newLink(), "me", &controls{}, set)
	other := newNode(newLink(), "them", &controls{}, set)
	p := newPeer(n.link, n.id, n.nonce, other.helloOf())
	return n, p
}

// Frames arrive whole and in order; a viewer joining mid-stream starts at
// the first key frame, not before.
func TestVideoReassemblesInOrder(t *testing.T) {
	n, p := videoNode(t)
	frame := func(i int) []byte {
		b := make([]byte, 3000+i)
		for j := range b {
			b[j] = byte(i + j)
		}
		return b
	}
	for _, pkt := range videoPackets(1, false, frame(1)) { // inter frame before any key: must be skipped
		n.gotVideo(p, pkt)
	}
	for id := uint32(2); id <= 4; id++ {
		for _, pkt := range videoPackets(id, id == 2, frame(int(id))) {
			n.gotVideo(p, pkt)
		}
	}
	rx := p.video.Load()
	if len(rx.out) != 3 {
		t.Fatalf("got %d frames, want 3 (key + 2)", len(rx.out))
	}
	for id := uint32(2); id <= 4; id++ {
		f := <-rx.out
		if f.id != id || string(f.data) != string(frame(int(id))) || f.key != (id == 2) {
			t.Fatalf("frame %d: id %d key %v len %d", id, f.id, f.key, len(f.data))
		}
	}
}

// A lost chunk drops that frame and everything after it until a key frame
// arrives, and the viewer asks for one.
func TestVideoRecoversAtKeyFrame(t *testing.T) {
	n, p := videoNode(t)
	data := make([]byte, 5000)
	for _, pkt := range videoPackets(1, true, data) {
		n.gotVideo(p, pkt)
	}
	pkts := videoPackets(2, false, data)
	for _, pkt := range pkts[1:] { // chunk 0 of frame 2 never comes
		n.gotVideo(p, pkt)
	}
	for _, pkt := range videoPackets(3, false, data) {
		n.gotVideo(p, pkt)
	}
	rx := p.video.Load()
	if len(rx.out) != 1 {
		t.Fatalf("got %d frames before the gap was noticed, want 1", len(rx.out))
	}
	rx.mu.Lock()
	rx.pending[2].since = time.Now().Add(-time.Second) // it has waited long enough
	rx.mu.Unlock()
	n.gotVideo(p, videoPackets(4, false, data)[0]) // any packet triggers the tidy-up
	rx.mu.Lock()
	synced, asked := rx.synced, !rx.keyAsk.IsZero()
	rx.mu.Unlock()
	if synced || !asked {
		t.Fatalf("after the gap: synced=%v asked=%v, want unsynced and a key frame request", synced, asked)
	}
	for _, pkt := range videoPackets(5, true, data) { // the key frame arrives
		n.gotVideo(p, pkt)
	}
	if len(rx.out) != 2 {
		t.Fatalf("got %d frames, want 2: the first key and the recovery key", len(rx.out))
	}
	<-rx.out
	if f := <-rx.out; f.id != 5 || !f.key {
		t.Fatalf("recovered on frame %d key=%v, want 5 key", f.id, f.key)
	}
}

func TestVideoKeyRequestReachesSender(t *testing.T) {
	n, p := videoNode(t)
	n.gotVideo(p, videoCtl(videoFlagKey))
	if n.keyReq.Load() != 1 {
		t.Fatalf("keyReq %d, want 1", n.keyReq.Load())
	}
}
