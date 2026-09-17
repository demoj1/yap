package main

import (
	"encoding/binary"
	"sync"
	"time"
)

// Screen sharing. The browser captures and encodes (VP8, WebCodecs); yap
// cuts each encoded frame into datagrams for everyone and puts frames
// back together on the other side, in order, for that browser to decode.
// A frame with a piece missing is dropped; the stream picks up again at
// the next key frame, which the viewer asks for.
//
// [8][frame id u32][chunk u16][chunks u16][flags u8][data]   flags bit 0: key frame
// chunks == 0 is a viewer talking to the sender: flags bit 0 asks for a
// key frame, bit 1 says "I am watching" (sent every second; frames go
// only to whoever said so lately).

const (
	videoChunk    = 1200                   // bytes of frame per datagram: relayed and sealed it stays under the MTU
	videoPace     = 250 * time.Microsecond // between datagrams to one viewer: ~40 Mbit/s ceiling
	videoQueue    = 4                      // frames waiting to go out; beyond that the newest wins
	videoWait     = 120 * time.Millisecond // how long a frame may stay incomplete before it is dropped
	videoKeyEvery = 500 * time.Millisecond // at most one key frame request per this
	videoWatchFor = 3 * time.Second        // a viewer's heartbeat keeps frames coming this long
	videoFlagKey  = 1
	videoFlagWant = 2
)

// videoCtl is a viewer's word to the sender: flags videoFlagKey / videoFlagWant.
func videoCtl(flags byte) []byte { return []byte{typVideo, 0, 0, 0, 0, 0, 0, 0, 0, flags} }

// watchingNow reports whether p asked for our screen in the last videoWatchFor.
func (p *peer) watchingNow() bool {
	at := p.watching.Load()
	return at != 0 && time.Since(time.Unix(0, at)) < videoWatchFor
}

// watchers counts the people our screen currently goes to.
func (n *node) watchers() int {
	c := 0
	for _, p := range n.people() {
		if p.watchingNow() {
			c++
		}
	}
	return c
}

type videoFrame struct {
	id   uint32
	key  bool
	data []byte
}

// videoRx puts one sender's frames back together and hands them out in order.
type videoRx struct {
	mu      sync.Mutex
	pending map[uint32]*videoPartial
	next    uint32 // frame id the viewer expects; 0 before the first key frame
	synced  bool   // a key frame has been delivered, inter frames make sense
	out     chan videoFrame
	keyAsk  time.Time
}

type videoPartial struct {
	key    bool
	chunks [][]byte
	got    int
	since  time.Time
}

func newVideoRx() *videoRx {
	return &videoRx{pending: map[uint32]*videoPartial{}, out: make(chan videoFrame, 64)}
}

// sendVideo queues one encoded frame for everyone; frames it cannot keep
// up with are dropped rather than delayed.
func (n *node) sendVideo(data []byte, key bool) {
	n.videoMu.Lock()
	if n.videoOut == nil {
		n.videoOut = make(chan videoFrame, videoQueue)
		go n.videoSender()
	}
	n.videoSeq++
	f := videoFrame{n.videoSeq, key, data}
	n.videoMu.Unlock()
	select {
	case n.videoOut <- f:
	default: // behind: drop the oldest waiting frame, keep this one
		select {
		case <-n.videoOut:
		default:
		}
		n.videoOut <- f
	}
}

func (n *node) videoSender() {
	for f := range n.videoOut {
		chunks := (len(f.data) + videoChunk - 1) / videoChunk
		for c := 0; c < chunks; c++ {
			pkt := make([]byte, 0, 10+videoChunk)
			pkt = append(pkt, typVideo)
			pkt = binary.BigEndian.AppendUint32(pkt, f.id)
			pkt = binary.BigEndian.AppendUint16(pkt, uint16(c))
			pkt = binary.BigEndian.AppendUint16(pkt, uint16(chunks))
			flags := byte(0)
			if f.key {
				flags = videoFlagKey
			}
			pkt = append(pkt, flags)
			pkt = append(pkt, f.data[c*videoChunk:min(len(f.data), (c+1)*videoChunk)]...)
			for _, p := range n.people() {
				if p.connected() && p.watchingNow() {
					n.sendTo(p, pkt)
				}
			}
			time.Sleep(videoPace)
		}
	}
}

// gotVideo handles one typVideo packet from p.
func (n *node) gotVideo(p *peer, plain []byte) {
	if len(plain) < 10 {
		return
	}
	id := binary.BigEndian.Uint32(plain[1:])
	chunk, chunks := int(binary.BigEndian.Uint16(plain[5:])), int(binary.BigEndian.Uint16(plain[7:]))
	if chunks == 0 { // a viewer talking to us
		flags := plain[9]
		if flags&videoFlagWant != 0 {
			if !p.watchingNow() { // a new viewer needs a key frame to start on
				n.keyReq.Add(1)
			}
			p.watching.Store(time.Now().UnixNano())
		}
		if flags&videoFlagKey != 0 {
			n.keyReq.Add(1)
		}
		return
	}
	rx := p.video.Load()
	if rx == nil {
		rx = newVideoRx()
		p.video.Store(rx)
	}
	rx.mu.Lock()
	defer rx.mu.Unlock()
	if rx.synced && id < rx.next {
		return // too old
	}
	part := rx.pending[id]
	if part == nil {
		part = &videoPartial{key: plain[9]&videoFlagKey != 0, chunks: make([][]byte, chunks), since: time.Now()}
		rx.pending[id] = part
	}
	if chunk >= len(part.chunks) || part.chunks[chunk] != nil {
		return
	}
	part.chunks[chunk] = append([]byte(nil), plain[10:]...)
	part.got++
	rx.deliver(n, p)
}

// deliver hands out every complete frame that is next in line, jumps to a
// complete key frame when the line is broken, and asks for a key frame
// when it has been waiting too long.
func (rx *videoRx) deliver(n *node, p *peer) {
	for {
		if !rx.synced { // start on any complete key frame
			for id, part := range rx.pending {
				if part.key && part.got == len(part.chunks) {
					rx.synced, rx.next = true, id
					break
				}
			}
			if !rx.synced {
				rx.tidy(n, p)
				return
			}
		}
		part := rx.pending[rx.next]
		if part != nil && part.got == len(part.chunks) {
			data := make([]byte, 0, len(part.chunks)*videoChunk)
			for _, c := range part.chunks {
				data = append(data, c...)
			}
			delete(rx.pending, rx.next)
			select {
			case rx.out <- videoFrame{rx.next, part.key, data}:
			default: // the browser is not reading: drop, it will resync
			}
			rx.next++
			continue
		}
		// The next frame is missing or incomplete. A newer complete key
		// frame lets us jump; otherwise wait a little, then give up on it.
		for id, other := range rx.pending {
			if id > rx.next && other.key && other.got == len(other.chunks) {
				for old := range rx.pending {
					if old < id {
						delete(rx.pending, old)
					}
				}
				rx.next = id
				break
			}
		}
		if rx.pending[rx.next] != nil && rx.pending[rx.next].got == len(rx.pending[rx.next].chunks) {
			continue
		}
		rx.tidy(n, p)
		return
	}
}

// tidy drops what has waited too long and, if that broke the stream, asks
// for a key frame (not more often than videoKeyEvery).
func (rx *videoRx) tidy(n *node, p *peer) {
	broken := !rx.synced
	for id, part := range rx.pending {
		if time.Since(part.since) > videoWait && part.got < len(part.chunks) {
			delete(rx.pending, id)
			if rx.synced && id >= rx.next {
				broken = true
			}
		}
	}
	if !broken {
		return
	}
	rx.synced = false
	if time.Since(rx.keyAsk) > videoKeyEvery {
		rx.keyAsk = time.Now()
		n.sendTo(p, videoCtl(videoFlagKey|videoFlagWant))
	}
}

// sharingNow reports whether p says they are sharing their screen.
func (p *peer) sharingNow() bool { return p.sharing.Load() }
