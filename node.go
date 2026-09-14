package main

import (
	"context"
	"encoding/binary"
	"log"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/demoj1/yap/internal/rnnoise"
	"github.com/gen2brain/malgo"
	"github.com/pion/stun/v3"
)

const (
	punchTimeout  = 20 * time.Second
	announceEvery = 20 * time.Second // re-announce so latecomers find us and NAT mappings stay warm
	retryPause    = 3 * time.Second
	statsEvery    = 5 * time.Second
)

var stunServers = []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302", "stun.sipgate.net:3478"}

// node is one participant of the mesh. It owns the socket and the audio
// device for the whole run, keeps a peer per other participant, fans the
// microphone out to all of them and mixes everything it hears into one
// playback stream.
type node struct {
	conn  *net.UDPConn
	audio *audio
	link  link
	id    []byte // random per run; orders the pair direction bit
	nonce []byte // random per run; halves of every pair key
	name  string
	ctl   *controls
	set   *settings

	mu     sync.Mutex
	peers  map[string]*peer // by string(id)
	byAddr map[string]*peer // source address → peer, once a packet authenticated
	joined int64            // monotonic, so the roster keeps join order

	stunCh chan []byte // STUN replies, routed out of recvLoop
	pub    atomic.Pointer[net.UDPAddr]
	room   *room
}

func newNode(l link, name string, ctl *controls, set *settings) *node {
	return &node{link: l, name: name, ctl: ctl, set: set,
		id: randBytes(8), nonce: randBytes(16),
		peers: map[string]*peer{}, byAddr: map[string]*peer{},
		stunCh: make(chan []byte, 4)}
}

// run blocks for the life of the process: it starts the socket/audio loops
// and then sits in the rendezvous, adding a peer for every hello it sees.
func (n *node) run() {
	go n.recvLoop()
	go n.sendLoop()
	go n.mixLoop()
	go n.reaper()
	go n.statsLoop()
	n.rendezvous()
}

func (n *node) rendezvous() {
	n.room = newRoom(n.link)
	for {
		ctx, cancel := context.WithCancel(context.Background())
		hellos, err := n.room.listen(ctx)
		if err != nil {
			cancel()
			log.Println("rendezvous:", err)
			time.Sleep(retryPause)
			continue
		}
		n.announce()
		tick := time.NewTicker(announceEvery)
	stream:
		for {
			select {
			case h, ok := <-hellos:
				if !ok {
					break stream // ntfy dropped the stream; resubscribe
				}
				if n.onHello(h) {
					n.announce() // a newcomer can't know us yet: answer right away
				}
			case <-tick.C:
				n.announce()
			}
		}
		tick.Stop()
		cancel()
		log.Println("rendezvous stream ended, reconnecting")
		time.Sleep(retryPause)
	}
}

func (n *node) announce() {
	prev := n.pub.Load()
	if pub, err := n.publicAddr(); err == nil {
		n.pub.Store(pub)
	} else if prev == nil {
		log.Println("stun:", err, "— only LAN addresses will be announced")
	}
	h := hello{ID: n.id, Name: n.name, Nonce: n.nonce, Addrs: candidates(n.conn, n.pub.Load())}
	for _, p := range n.peerList() {
		if p.direct() {
			h.Reach = append(h.Reach, p.id)
		}
	}
	if cur := n.pub.Load(); prev == nil || cur == nil || cur.String() != prev.String() {
		log.Println("you are at", h.Addrs)
	}
	if err := n.room.say(h); err != nil {
		log.Println("rendezvous:", err)
	}
}

// onHello adds or refreshes a peer; it reports true when the hello came
// from someone we had not seen in this run.
func (n *node) onHello(h hello) bool {
	if string(h.ID) == string(n.id) {
		return false
	}
	if len(h.Nonce) == 0 || len(h.ID) == 0 {
		log.Println("someone with an old yap is in the room — ask them to update")
		return false
	}
	n.mu.Lock()
	old, known := n.peers[string(h.ID)]
	if known && string(old.nonce) == string(h.Nonce) {
		n.mu.Unlock()
		old.reach.Store(&h.Reach)
		if !old.direct() && old.via.Load() == nil {
			go n.punch(old, h.Addrs) // still trying; their fresh candidates may help
		}
		return false
	}
	if known { // same person, new run: drop the stale pair key
		n.dropLocked(old)
	}
	p := newPeer(n.link, n.id, n.nonce, h)
	p.volume.Store(int32(n.set.volume(p.name)))
	p.reach.Store(&h.Reach)
	n.joined++
	p.joinedAt = n.joined
	p.since = time.Now()
	n.peers[string(h.ID)] = p
	n.mu.Unlock()
	log.Println(h.Name, "is at", h.Addrs)
	go n.punch(p, h.Addrs)
	return true
}

// relayFor picks a directly connected peer that says it reaches p, so audio
// for p can go through it. The link owner with an open port is the usual
// candidate: everyone reaches it.
func (n *node) relayFor(p *peer) *peer {
	for _, r := range n.peerList() {
		if r != p && r.direct() && r.reaches(p.id) {
			return r
		}
	}
	return nil
}

// punch pings every candidate address of the peer until one of its packets
// gets through, then just returns; the peer is dropped if nothing arrives.
func (n *node) punch(p *peer, cands []string) {
	if !p.punching.CompareAndSwap(false, true) {
		return
	}
	defer p.punching.Store(false)
	var addrs []*net.UDPAddr
	for _, c := range cands {
		if a, err := net.ResolveUDPAddr("udp4", c); err == nil {
			addrs = append(addrs, a)
		}
	}
	deadline := time.After(punchTimeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, a := range addrs {
			n.conn.WriteToUDP(p.seal(nil), a)
		}
		select {
		case <-p.ready:
			if p.direct() {
				log.Println("connected:", p.name, p.addr.Load())
			} else if via := p.via.Load(); via != nil {
				log.Println("connected:", p.name, "via", via.name)
			}
			return
		case <-p.gone:
			return
		case <-deadline:
			if r := n.relayFor(p); r != nil {
				p.via.Store(r)
				log.Println("no direct path to", p.name, "— relaying via", r.name)
				return
			}
			// Nobody can relay yet; keep the peer so a relayed packet from
			// their side can still land. expire() drops it if nothing comes.
			log.Println("could not reach", p.name, "(symmetric NAT on one side?)")
			return
		case <-tick.C:
		}
	}
}

func (n *node) drop(p *peer) {
	n.mu.Lock()
	n.dropLocked(p)
	n.mu.Unlock()
}

func (n *node) dropLocked(p *peer) {
	if n.peers[string(p.id)] == p {
		delete(n.peers, string(p.id))
	}
	for k, q := range n.byAddr {
		if q == p {
			delete(n.byAddr, k)
		}
	}
	for _, q := range n.peers {
		if q.via.Load() == p {
			q.via.Store(nil) // their relay is gone; the next hello re-punches
		}
	}
	p.markGone()
}

// recvLoop is the only reader of the socket. It routes STUN replies to the
// rendezvous and every other packet to whichever peer can authenticate it.
func (n *node) recvLoop() {
	buf := make([]byte, 1500)
	for {
		c, from, err := n.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := buf[:c]
		if isSTUN(pkt) {
			select {
			case n.stunCh <- append([]byte(nil), pkt...):
			default:
			}
			continue
		}
		key := from.String()
		n.mu.Lock()
		p := n.byAddr[key]
		n.mu.Unlock()
		if p != nil {
			if plain, ok := p.open(pkt); ok {
				n.dispatch(p, from, pkt, plain)
				continue
			}
		}
		for _, q := range n.peerList() {
			if plain, ok := q.open(pkt); ok {
				n.mu.Lock()
				n.byAddr[key] = q
				n.mu.Unlock()
				n.dispatch(q, from, pkt, plain)
				break
			}
		}
	}
}

// dispatch handles an authenticated packet from peer q at addr from: audio
// for the mixer, a forward request to pass on, or a relayed packet from a
// third peer to unwrap.
func (n *node) dispatch(q *peer, from *net.UDPAddr, pkt, plain []byte) {
	seq := binary.BigEndian.Uint64(pkt[:8])
	if len(plain) == 0 {
		q.accept(from, seq, nil)
		return
	}
	switch plain[0] {
	case typAudio:
		q.accept(from, seq, plain[1:])
	case typForward:
		q.accept(from, seq, nil)
		if len(plain) < 1+idLen+8 {
			return
		}
		dst := n.peerByID(plain[1 : 1+idLen])
		if dst == nil || !dst.direct() {
			return
		}
		out := make([]byte, 0, 1+idLen+len(plain)-1-idLen)
		out = append(out, typRelayed)
		out = append(out, q.id...)
		out = append(out, plain[1+idLen:]...)
		n.conn.WriteToUDP(dst.seal(out), dst.addr.Load())
	case typRelayed:
		q.accept(from, seq, nil)
		if len(plain) < 1+idLen+8 {
			return
		}
		src := n.peerByID(plain[1 : 1+idLen])
		if src == nil {
			return
		}
		inner := plain[1+idLen:]
		innerPlain, ok := src.open(inner)
		if !ok {
			return
		}
		if !src.direct() && src.via.Load() == nil {
			src.via.Store(q) // they found a relay to us; answer the same way
		}
		innerSeq := binary.BigEndian.Uint64(inner[:8])
		if len(innerPlain) == 0 {
			src.accept(nil, innerSeq, nil)
		} else if innerPlain[0] == typAudio {
			src.accept(nil, innerSeq, innerPlain[1:])
		}
	}
}

func (n *node) peerByID(id []byte) *peer {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.peers[string(id)]
}

func isSTUN(pkt []byte) bool {
	return len(pkt) >= 20 && binary.BigEndian.Uint32(pkt[4:8]) == 0x2112A442
}

// sendLoop runs for the life of the node so microphone frames are always
// drained; each frame is encoded once and sealed separately for every
// connected peer.
func (n *node) sendLoop() {
	enc := newEncoder()
	dn := rnnoise.New()
	defer dn.Close()
	bitrate := 0
	for f := range n.audio.frames {
		if n.ctl.muted.Load() {
			clear(f)
		} else if n.ctl.denoise.Load() {
			dn.Process(f[:rnnoise.FrameSize])
			dn.Process(f[rnnoise.FrameSize:])
		}
		n.audio.micPeak.observe(f)
		peers := n.peerList()
		if len(peers) == 0 {
			continue
		}
		if b := int(n.ctl.bitrate.Load()); b != bitrate {
			must(enc.enc.SetBitrate(b * 1000))
			bitrate = b
		}
		audio := append([]byte{typAudio}, enc.encode(f)...)
		for _, p := range peers {
			n.sendTo(p, audio)
		}
	}
}

// sendTo delivers one payload to p: directly when we have their address,
// otherwise wrapped as a forward request to their relay. Nothing is sent
// while neither exists.
func (n *node) sendTo(p *peer, payload []byte) {
	var pkt []byte
	var to *net.UDPAddr
	if addr := p.addr.Load(); addr != nil {
		pkt, to = p.seal(payload), addr
	} else if via := p.via.Load(); via != nil && via.direct() {
		inner := p.seal(payload)
		fwd := make([]byte, 0, 1+idLen+len(inner))
		fwd = append(fwd, typForward)
		fwd = append(fwd, p.id...)
		fwd = append(fwd, inner...)
		pkt, to = via.seal(fwd), via.addr.Load()
	} else {
		return
	}
	if _, err := n.conn.WriteToUDP(pkt, to); err != nil {
		log.Println("send:", err)
	}
	p.tx.Add(1)
	p.txBytes.Add(uint64(len(pkt)))
}

// mixLoop feeds the playback queue: one frame from every peer, scaled by
// that peer's volume, summed and clamped.
func (n *node) mixLoop() {
	mix := make([]int32, frameSize)
	out := make([]int16, frameSize)
	var lim limiter
	for range n.audio.play.need {
		for n.audio.play.len() < playTarget*frameSize {
			clear(mix)
			got := false
			for _, p := range n.peerList() {
				pcm := p.nextFrame()
				if pcm == nil {
					continue
				}
				got = true
				p.level.observe(pcm)
				mixInto(mix, pcm, p.volume.Load())
			}
			if !got {
				break
			}
			lim.apply(mix, out)
			n.audio.spkPeak.observe(out)
			n.audio.play.push(out)
		}
	}
}

const neverConnectedTimeout = 60 * time.Second

func (n *node) reaper() {
	for range time.Tick(time.Second) {
		for _, p := range n.peerList() {
			switch {
			case p.connected() && p.silentFor() > peerTimeout:
				log.Println(p.name, "is gone")
				n.drop(p)
			case !p.connected() && time.Since(p.since) > neverConnectedTimeout:
				log.Println("giving up on", p.name, "— will retry on their next hello")
				n.drop(p)
			}
		}
	}
}

func (n *node) statsLoop() {
	for range time.Tick(statsEvery) {
		for _, p := range n.peerList() {
			if p.connected() {
				log.Println(p.stats(statsEvery))
			}
		}
	}
}

// peerList is a snapshot of the roster in join order.
func (n *node) peerList() []*peer {
	n.mu.Lock()
	out := make([]*peer, 0, len(n.peers))
	for _, p := range n.peers {
		out = append(out, p)
	}
	n.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].joinedAt < out[j].joinedAt })
	return out
}

// setVolume updates a peer's live volume and remembers it by name.
func (n *node) setVolume(p *peer, v int) {
	p.volume.Store(int32(v))
	n.set.setVolume(p.name, v)
}

// publicAddr asks STUN servers how this socket looks from the internet.
// Several are tried because e.g. Google is blocked on some networks.
func (n *node) publicAddr() (addr *net.UDPAddr, err error) {
	for _, s := range stunServers {
		if addr, err = n.stunQuery(s); err == nil {
			return addr, nil
		}
	}
	return nil, err
}

func (n *node) stunQuery(server string) (*net.UDPAddr, error) {
	srv, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, err
	}
	req := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	for len(n.stunCh) > 0 { // stale replies from an earlier query
		<-n.stunCh
	}
	if _, err := n.conn.WriteToUDP(req.Raw, srv); err != nil {
		return nil, err
	}
	select {
	case raw := <-n.stunCh:
		msg := &stun.Message{Raw: raw}
		if err := msg.Decode(); err != nil {
			return nil, err
		}
		var xor stun.XORMappedAddress
		if err := xor.GetFrom(msg); err != nil {
			return nil, err
		}
		return &net.UDPAddr{IP: xor.IP, Port: xor.Port}, nil
	case <-time.After(2 * time.Second):
		return nil, context.DeadlineExceeded
	}
}

// cycleDevice switches the mic (kind Capture) or speaker (kind Playback) to
// the next available one, wrapping through "" = system default, and
// remembers the choice. Returns a short label for the UI.
func (n *node) cycleDevice(kind malgo.DeviceType) string {
	devs, err := listDevices(kind)
	if err != nil {
		log.Println("devices:", err)
		return ""
	}
	names := []string{""} // "" = system default, always first
	for i := range devs {
		if kind == malgo.Capture && isMonitor(devs[i].Name()) {
			continue // loopback monitors are never a usable microphone
		}
		names = append(names, devs[i].Name())
	}
	cur := n.audio.mic
	if kind == malgo.Playback {
		cur = n.audio.out
	}
	idx := 0
	for i, name := range names {
		if name == cur {
			idx = i
			break
		}
	}
	next := names[(idx+1)%len(names)]

	var mic, out string
	if kind == malgo.Capture {
		mic, out = next, n.audio.out
	} else {
		mic, out = n.audio.mic, next
	}
	gotMic, gotOut, err := n.audio.reopen(mic, out)
	if err != nil {
		log.Println("switch device:", err)
	}
	n.set.Mic, n.set.Out = gotMic, gotOut
	n.set.save()
	if kind == malgo.Capture {
		return "mic: " + orDefault(gotMic)
	}
	return "out: " + orDefault(gotOut)
}
