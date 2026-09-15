package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/demoj1/yap/internal/rnnoise"
	"github.com/gen2brain/malgo"
	"github.com/pion/stun/v3"
)

const (
	punchTimeout  = 20 * time.Second
	announceEvery = 45 * time.Second // re-announce so latecomers find us and NAT mappings stay warm
	retryPause    = 8 * time.Second
	statsEvery    = 5 * time.Second
)

var stunServers = []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302", "stun.sipgate.net:3478"}

// node is one participant of the mesh. It owns the socket and the audio
// device for the whole run, keeps a peer per other participant, fans the
// microphone out to all of them and mixes everything it hears into one
// playback stream.
type node struct {
	conn   *net.UDPConn
	audio  *audio
	talkMS atomic.Int64 // milliseconds of non-silent frames we sent: our talk time

	gateOpen atomic.Bool   // the noise gate let the last frame through
	agcGain  atomic.Uint64 // float64 bits: the gain AGC applied to the last frame
	link     link
	id       []byte // random per run; orders the pair direction bit
	nonce    []byte // random per run; halves of every pair key
	name     string
	ctl      *controls
	set      *settings

	mu     sync.Mutex
	peers  map[string]*peer // by string(id)
	byAddr map[string]*peer // source address → peer, once a packet authenticated
	joined int64            // monotonic, so the roster keeps join order

	stunCh  chan []byte // STUN replies, routed out of recvLoop
	cues    chan int    // join/leave chimes queued for the mixer
	pub     atomic.Pointer[net.UDPAddr]
	room    *room
	update  atomic.Pointer[string]          // "update available ..." once the check found a newer release
	roster  atomic.Pointer[[]*peer]         // cached sorted snapshot for the per-frame hot paths
	folks   atomic.Pointer[[]*peer]         // roster minus relays: the people we talk to
	lastSay atomic.Int64                    // unix nanos of the last announce; throttles vs ntfy 429
	relay   bool                            // relay/daemon mode: no audio, just forward for everyone
	locked  atomic.Bool                     // room lock: no new participants admitted
	allowed atomic.Pointer[map[string]bool] // names admitted at lock time; nil when unlocked
}

func newNode(l link, name string, ctl *controls, set *settings) *node {
	return &node{link: l, name: name, ctl: ctl, set: set,
		id: randBytes(8), nonce: randBytes(16),
		peers: map[string]*peer{}, byAddr: map[string]*peer{},
		stunCh: make(chan []byte, 4), cues: make(chan int, 8)}
}

const (
	cueJoin  = 1
	cueLeave = 2
)

// chime synthesizes the join (rising) or leave (falling) two-note cue:
// 80 ms per note, 5 ms fades, about -15 dBFS.
func chime(kind int) []int16 {
	notes := [2]float64{660, 880}
	if kind == cueLeave {
		notes = [2]float64{880, 660}
	}
	const n, fade, amp = sampleRate * 80 / 1000, sampleRate * 5 / 1000, 6000
	out := make([]int16, 0, 2*n)
	for _, f := range notes {
		for i := 0; i < n; i++ {
			env := 1.0
			if i < fade {
				env = float64(i) / fade
			} else if i > n-fade {
				env = float64(n-i) / fade
			}
			out = append(out, int16(amp*env*math.Sin(2*math.Pi*f*float64(i)/sampleRate)))
		}
	}
	return out
}

// cue queues a chime; dropped if the mixer is behind, a chime is not worth waiting for.
func (n *node) cue(kind int) {
	select {
	case n.cues <- kind:
	default:
	}
}

// run blocks for the life of the process: it starts the socket/audio loops
// and then sits in the rendezvous, adding a peer for every hello it sees.
func (n *node) run() {
	go n.recvLoop()
	go n.sendLoop()
	go n.mixLoop()
	go n.reaper()
	go n.statsLoop()
	go n.pingLoop()
	n.rendezvous()
}

// runRelay is the daemon mode: no microphone, no playback, no TUI. It just
// keeps an open port, punches to everyone so it becomes a direct peer of all,
// and forwards packets between participants who cannot reach each other. On a
// public, always-on host it is a stable relay hub and rendezvous anchor.
func (n *node) runRelay() {
	n.relay = true
	go autoUpdate(func() bool { // busy: anyone (not a relay) is connected through us right now
		for _, p := range n.peerList() {
			if !p.relay && p.connected() && p.silentFor() < peerTimeout {
				return true
			}
		}
		return false
	})
	go n.recvLoop()
	go n.reaper()
	go n.statsLoop()
	go n.pingLoop() // keepalives on each peer's learned source addr: completes
	// the reverse path to symmetric-NAT clients (they reach our open port, but
	// we never reply otherwise) and holds the hole open.
	n.rendezvous()
}

// pingLoop measures round trip to every connected peer once a second over
// whatever path audio takes (direct or relayed).
func (n *node) pingLoop() {
	for range time.Tick(time.Second) {
		for _, p := range n.peerList() {
			if p.connected() {
				n.sendTo(p, stampPayload(typPing, time.Now().UnixNano()))
			}
		}
		n.sendState()
	}
}

func (n *node) rendezvous() {
	n.room = newRoom(n.link)
	for {
		ctx, cancel := context.WithCancel(context.Background())
		hellos, err := n.room.listen(ctx)
		if err != nil {
			cancel()
			log.Println("rendezvous:", err)
			pause := retryPause
			if strings.Contains(err.Error(), "429") {
				pause = backoff429
				log.Println("ntfy rate-limited the subscription — backing off", pause)
			}
			time.Sleep(pause)
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

const (
	minAnnounceGap = 10 * time.Second // ntfy free tier rate-limits; don't hammer it
	backoff429     = 90 * time.Second // when ntfy says 429, wait this long before the next POST
)

func (n *node) announce() {
	last := n.lastSay.Load()
	if last != 0 && time.Since(time.Unix(0, last)) < minAnnounceGap {
		return
	}
	n.lastSay.Store(time.Now().UnixNano())
	prev := n.pub.Load()
	if pub, err := n.publicAddr(); err == nil {
		n.pub.Store(pub)
	} else if prev == nil {
		log.Println("stun:", err, "— only LAN addresses will be announced")
	}
	h := hello{Proto: proto, ID: n.id, Name: n.name, Nonce: n.nonce, Addrs: candidates(n.conn, n.pub.Load()), Relay: n.relay, Ver: version}
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
		wait := 20 * time.Second
		if strings.Contains(err.Error(), "429") {
			wait = backoff429
			log.Println("ntfy rate-limited us — backing off", wait)
		}
		n.lastSay.Store(time.Now().Add(wait - minAnnounceGap).UnixNano())
	}
}

// onHello adds or refreshes a peer; it reports true when the hello came
// from someone we had not seen in this run.
func (n *node) onHello(h hello) bool {
	if string(h.ID) == string(n.id) {
		return false
	}
	if h.Proto != proto || len(h.Nonce) == 0 || len(h.ID) == 0 {
		who := h.Name
		if who == "" {
			who = "someone"
		}
		log.Printf("%s runs an incompatible yap (proto %d, need %d) — ask them to update", who, h.Proto, proto)
		return false
	}
	if n.locked.Load() && !h.Relay { // relays only forward; a lock is about people
		if a := n.allowed.Load(); a == nil || !(*a)[h.Name] {
			return false // room is locked to newcomers (matched by name, so a reconnect is let back in)
		}
	}
	if h.Name == n.name { // our own previous run, replayed from the rendezvous cache
		return false
	}
	n.mu.Lock()
	old, known := n.peers[string(h.ID)]
	if known && string(old.nonce) == string(h.Nonce) {
		n.mu.Unlock()
		old.reach.Store(&h.Reach)
		if !old.direct() {
			go n.punch(old, h.Addrs) // relayed or not: keep trying for a direct path with their fresh candidates
		}
		return false
	}
	if known { // same person, new run: drop the stale pair key
		n.dropLocked(old)
	}
	// A new run under a known name: IDs are per run, so the name is how we
	// tell that this is the same person restarted. The cache replays in
	// time order and the living re-announce, so the latest hello wins.
	for _, q := range n.peers {
		if q.name == h.Name {
			log.Println(h.Name, "restarted")
			n.dropLocked(q)
		}
	}
	p := newPeer(n.link, n.id, n.nonce, h)
	p.volume.Store(int32(n.set.volume(p.name)))
	p.reach.Store(&h.Reach)
	n.joined++
	p.joinedAt = n.joined
	p.since = time.Now()
	n.peers[string(h.ID)] = p
	n.rebuildRoster()
	n.mu.Unlock()
	log.Println(h.Name, "is at", h.Addrs)
	go n.punch(p, h.Addrs)
	return true
}

// relayFor picks a directly connected peer to carry audio for p: a dedicated
// relay (its port is open, so everyone reaches it — no need to wait for its
// hello to list p), or a person who says they reach p. Dedicated relays win
// over people, and among equals the lowest round trip; an unmeasured one
// ranks last.
func (n *node) relayFor(p *peer) *peer {
	better := func(a, b *peer) bool { // a relay beats a person; then a measured, lower round trip
		if a.relay != b.relay {
			return a.relay
		}
		ra, rb := a.rttUS.Load(), b.rttUS.Load()
		if ra == 0 || rb == 0 {
			return ra != 0
		}
		return ra < rb
	}
	var best *peer
	for _, r := range n.peerList() {
		if r != p && r.direct() && (r.relay || r.reaches(p.id)) && (best == nil || better(r, best)) {
			best = r
		}
	}
	return best
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
	// Fastest path first: if a relay is already at hand, audio goes through
	// it right now, and the direct punch below runs in the background —
	// sendTo switches to the direct address the moment it appears.
	fresh := !p.connected()
	if p.via.Load() == nil {
		if r := n.relayFor(p); r != nil {
			p.via.Store(r)
			log.Println("reaching", p.name, "via", r.name, "— trying a direct path in the background")
		}
	}
	deadline := time.After(punchTimeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	ready := p.ready // fires once; nil afterwards so the loop keeps punching
	for {
		for _, a := range addrs {
			n.conn.WriteToUDP(p.seal(nil), a)
		}
		select {
		case <-ready:
			ready = nil
			if p.direct() {
				log.Println("connected:", p.name, p.addr.Load())
				if p.relay {
					n.adoptRelay(p)
				}
				return
			}
			if via := p.via.Load(); via != nil && fresh {
				log.Println("connected:", p.name, "via", via.name)
			}
		case <-p.gone:
			return
		case <-deadline:
			if via := p.via.Load(); via != nil {
				if fresh {
					log.Println("no direct path to", p.name, "— staying via", via.name)
				}
				return
			}
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
			if p.direct() { // their own packet landed while we were relaying
				log.Println("connected:", p.name, p.addr.Load(), "— direct now")
				return
			}
		}
	}
}

// adoptRelay routes everyone still without a path through a relay that just
// became reachable, so a hello that arrived before the relay did is not
// stuck waiting out its direct punch.
func (n *node) adoptRelay(r *peer) {
	for _, q := range n.peerList() {
		if q != r && !q.direct() && q.via.Load() == nil && q.via.CompareAndSwap(nil, r) {
			log.Println("reaching", q.name, "via", r.name, "— trying a direct path in the background")
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
	n.rebuildRoster()
	for _, q := range n.peers {
		if q.via.Load() != p {
			continue
		}
		r := n.relayFor(q) // roster is already without p
		q.via.Store(r)     // nil: the next hello re-punches
		if r != nil {
			log.Println(p.name, "is gone — now relaying to", q.name, "via", r.name)
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
	if len(plain) == 0 {
		q.accept(from, 0, nil)
		return
	}
	switch plain[0] {
	case typForward:
		q.accept(from, 0, nil)
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
		q.accept(from, 0, nil)
		if len(plain) < 1+idLen+8 {
			return
		}
		src := n.peerByID(plain[1 : 1+idLen])
		if src == nil {
			return
		}
		innerPlain, ok := src.open(plain[1+idLen:])
		if !ok {
			return
		}
		if !src.direct() && src.via.Load() == nil {
			src.via.Store(q) // they found a relay to us; answer the same way
		}
		n.deliver(src, nil, innerPlain) // nil: the relay's address is not theirs
	default:
		n.deliver(q, from, plain)
	}
}

// deliver handles an end-to-end payload from p: audio for the mixer, or a
// ping/pong for the round-trip meter. from is nil when it came via a relay.
func (n *node) deliver(p *peer, from *net.UDPAddr, plain []byte) {
	if len(plain) == 0 {
		p.accept(from, 0, nil)
		return
	}
	switch plain[0] {
	case typAudio:
		if n.relay {
			p.accept(from, 0, nil) // relay keeps the peer alive but never buffers audio
			return
		}
		if frame, opus, ok := parseAudio(plain); ok {
			p.accept(from, frame, opus)
		}
	case typPing:
		p.accept(from, 0, nil)
		if ts, ok := parseStamp(plain); ok {
			n.sendTo(p, stampPayload(typPong, ts))
		}
	case typPong:
		p.accept(from, 0, nil)
		if ts, ok := parseStamp(plain); ok {
			p.gotPong(ts)
		}
	case typState:
		p.accept(from, 0, nil)
		if len(plain) >= 2 {
			p.muted.Store(plain[1]&stateMuted != 0)
		}
		if len(plain) >= 3 { // v0.8.4+: they also say whether our packets reach them
			p.hearsUs.Store(plain[2]&stateHears != 0)
			p.stateAt.Store(time.Now().UnixNano())
		}
	}
}

// sendState tells every connected peer whether our mic is off right now and
// whether we have been hearing from them, so their tile of us can say
// "muted" or "not heard". Called on every change and once a second.
func (n *node) sendState() {
	if n.relay {
		return
	}
	var flags byte
	if n.ctl.silenced() {
		flags |= stateMuted
	}
	for _, p := range n.people() {
		if !p.connected() {
			continue
		}
		var link byte
		if p.silentFor() < noReplyAfter {
			link |= stateHears
		}
		n.sendTo(p, []byte{typState, flags, link})
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
	var g gate
	var ag agc
	var payload []byte // reused each frame
	bitrate := 0
	var frame uint32 // audio frame number, shared by every peer's copy of this frame
	for f := range n.audio.frames {
		quiet := n.ctl.silenced()
		if quiet {
			clear(f)
		} else if n.ctl.denoise.Load() {
			dn.Process(f[:rnnoise.FrameSize])
			dn.Process(f[rnnoise.FrameSize:])
		}
		if n.ctl.agc.Load() && !quiet {
			ag.process(f) // normalize outgoing loudness
		}
		n.agcGain.Store(math.Float64bits(ag.gain))
		n.audio.micPeak.observe(f)
		open := !quiet
		if n.ctl.gate.Load() && !quiet && !g.pass(f) {
			clear(f) // below the gate: send silence so speaker echo isn't transmitted
			open = false
		}
		n.gateOpen.Store(open)
		if !isQuiet(f) {
			n.talkMS.Add(frameMS)
		}
		people := n.people() // relays only forward; audio addressed to them is dropped
		if len(people) == 0 {
			continue
		}
		if b := int(n.ctl.bitrate.Load()); b != bitrate {
			must(enc.enc.SetBitrate(b * 1000))
			bitrate = b
		}
		payload = appendAudio(payload, frame, enc.encode(f))
		frame++
		for _, p := range people {
			n.sendTo(p, payload)
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
	var cue []int16 // pending chime samples, mixed in a frame at a time
	cueFrame := make([]int16, frameSize)
	for range n.audio.play.need {
		for n.audio.play.len() < playTarget*frameSize {
			clear(mix)
			got := false
			select {
			case k := <-n.cues:
				cue = append(cue, chime(k)...)
			default:
			}
			if len(cue) > 0 {
				clear(cueFrame)
				cue = cue[copy(cueFrame, cue):]
				mixInto(mix, cueFrame, 100)
				got = true
			}
			for _, p := range n.people() {
				pcm := p.nextFrame()
				if pcm == nil {
					continue
				}
				got = true
				p.level.observe(pcm)
				if !isQuiet(pcm) {
					p.talkMS.Add(frameMS)
				}
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

// peerList returns the current roster in join order. It hands back a cached
// immutable snapshot, so the per-frame callers (sendLoop, mixLoop, recvLoop)
// neither allocate nor sort; the slice is rebuilt only when the membership
// changes. Never mutate the returned slice.
func (n *node) peerList() []*peer {
	if r := n.roster.Load(); r != nil {
		return *r
	}
	return nil
}

// people is peerList without relays: those who send and hear audio.
func (n *node) people() []*peer {
	if r := n.folks.Load(); r != nil {
		return *r
	}
	return nil
}

// toggleLock locks or unlocks the room. Locking snapshots the current
// participants (plus us): while locked, hellos from anyone else are ignored,
// so no new person can join. Returns a line for the UI.
func (n *node) toggleLock() string {
	if n.locked.Load() {
		n.locked.Store(false)
		n.allowed.Store(nil)
		log.Println("room unlocked")
		return "room unlocked — anyone with the link can join"
	}
	allowed := map[string]bool{n.name: true}
	for _, p := range n.people() {
		allowed[p.name] = true
	}
	if len(allowed) < 2 {
		return "nobody here yet — lock once your people have joined"
	}
	n.allowed.Store(&allowed)
	n.locked.Store(true)
	log.Printf("room locked with %d participant(s)", len(allowed))
	return fmt.Sprintf("room LOCKED — %d here, no one new gets in", len(allowed))
}

// rebuildRoster refreshes the cached snapshot. Call with n.mu held after any
// add or remove.
func (n *node) rebuildRoster() {
	out := make([]*peer, 0, len(n.peers))
	for _, p := range n.peers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].joinedAt < out[j].joinedAt })
	n.roster.Store(&out)
	folks := make([]*peer, 0, len(out))
	for _, p := range out {
		if !p.relay {
			folks = append(folks, p)
		}
	}
	n.folks.Store(&folks)
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

// deviceNames lists what the picker offers for kind: "" (system default)
// first, then every device, minus loopback monitors for the microphone.
// cur is the index of the device in use.
func (n *node) deviceNames(kind malgo.DeviceType) (names []string, cur int) {
	devs, err := listDevices(kind)
	if err != nil {
		log.Println("devices:", err)
	}
	names = []string{""}
	for i := range devs {
		if kind == malgo.Capture && isMonitor(devs[i].Name()) {
			continue // loopback monitors are never a usable microphone
		}
		names = append(names, devs[i].Name())
	}
	using := n.audio.mic
	if kind == malgo.Playback {
		using = n.audio.out
	}
	for i, name := range names {
		if name == using {
			cur = i
		}
	}
	return names, cur
}

// useDevice switches the microphone or speaker live and remembers it.
func (n *node) useDevice(kind malgo.DeviceType, name string) string {
	mic, out := name, n.audio.out
	if kind == malgo.Playback {
		mic, out = n.audio.mic, name
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
