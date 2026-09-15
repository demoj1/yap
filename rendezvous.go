package main

import (
	"bufio"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// rendezvousHosts are ntfy-compatible servers used only to swap encrypted
// hellos. We publish to and subscribe from ALL of them at once, so two
// participants meet as long as any single host is reachable by both — one
// host rate-limiting us (429) or going down no longer breaks the call. The
// payload is AEAD-encrypted, so plain http mirrors are fine.
var rendezvousHosts = []string{
	"https://ntfy.sh",
	"https://ntfy.envs.net",
	"https://ntfy.adminforge.de",
}

const staleHello = 60 // seconds: a cached hello older than this belongs to a run that is gone

// hello is what every participant publishes to the room.
type hello struct {
	Proto int      `json:"proto"`
	ID    []byte   `json:"id"`
	Name  string   `json:"name"`
	Nonce []byte   `json:"nonce"`
	Addrs []string `json:"addrs"`
	Reach [][]byte `json:"reach,omitempty"`
	Relay bool     `json:"relay,omitempty"` // headless forwarder, not a person: no audio, never locked out
	Ver   string   `json:"ver,omitempty"`   // build tag, shown next to the name so mismatches are obvious
}

// room fans hellos out across every rendezvous host.
type room struct {
	topic string
	aead  cipher.AEAD
	hosts []string
}

func newRoom(l link) *room {
	key := l.sigKey()
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		panic(err)
	}
	return &room{topic: l.topic(), aead: aead, hosts: rendezvousHosts}
}

// say posts one hello to every host concurrently. It returns an error only
// if every host failed, so a 429 or outage on some hosts is not fatal.
func (r *room) say(h hello) error {
	plain, err := json.Marshal(h)
	if err != nil {
		panic(err)
	}
	nonce := make([]byte, chacha20poly1305.NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	body := base64.StdEncoding.EncodeToString(r.aead.Seal(nonce, nonce, plain, nil))

	errs := make(chan error, len(r.hosts))
	for _, host := range r.hosts {
		go func() { errs <- postTo(host+"/"+r.topic, body) }()
	}
	err = fmt.Errorf("no rendezvous hosts")
	for range r.hosts {
		if err = <-errs; err == nil {
			return nil
		}
	}
	return err
}

func postTo(url, body string) error {
	c := &http.Client{Timeout: 8 * time.Second}
	resp, err := c.Post(url, "text/plain", strings.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}

// listen subscribes to every host and merges their hellos onto one channel.
// Each host resubscribes on its own when its stream drops, so the room keeps
// working while individual hosts come and go. Duplicate hellos are harmless:
// onHello is idempotent per (id, nonce).
func (r *room) listen(ctx context.Context) <-chan hello {
	out := make(chan hello)
	for _, host := range r.hosts {
		go r.subscribe(ctx, host, out)
	}
	return out
}

func (r *room) subscribe(ctx context.Context, host string, out chan<- hello) {
	// since= replays the host's recent cache on connect: everyone announces
	// at least every announceEvery, so a newcomer learns the whole room the
	// moment it subscribes instead of waiting for the next round of hellos.
	url := host + "/" + r.topic + "/json?since=2m"
	for {
		if ctx.Err() != nil {
			return
		}
		wait := r.stream(ctx, url, out)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// stream holds one subscription open, feeding hellos to out, and returns how
// long to wait before reconnecting (longer after a 429 or error).
func (r *room) stream(ctx context.Context, url string, out chan<- hello) time.Duration {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 8 * time.Second
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return 90 * time.Second
	}
	if resp.StatusCode != 200 {
		return 15 * time.Second
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var ev struct {
		Event   string `json:"event"`
		Message string `json:"message"`
		Time    int64  `json:"time"` // unix seconds the host received it
	}
	for sc.Scan() {
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "message" {
			continue
		}
		// The replayed cache also holds hellos of runs that are already gone
		// (our own previous one included); everyone alive re-announces within
		// announceEvery, so anything older than that is a ghost.
		if ev.Time != 0 && time.Now().Unix()-ev.Time > staleHello {
			continue
		}
		if h, ok := r.open(ev.Message); ok {
			select {
			case out <- h:
			case <-ctx.Done():
				return 0
			}
		}
	}
	return 3 * time.Second // clean EOF: reconnect soon
}

func (r *room) open(msg string) (hello, bool) {
	raw, err := base64.StdEncoding.DecodeString(msg)
	if err != nil || len(raw) < chacha20poly1305.NonceSize {
		return hello{}, false
	}
	plain, err := r.aead.Open(nil, raw[:chacha20poly1305.NonceSize], raw[chacha20poly1305.NonceSize:], nil)
	if err != nil {
		return hello{}, false
	}
	var h hello
	if json.Unmarshal(plain, &h) != nil {
		return hello{}, false
	}
	return h, true
}

// candidates lists where this socket can be reached: every LAN IPv4 plus
// the STUN-mapped public address, if known.
func candidates(conn *net.UDPConn, pub *net.UDPAddr) []string {
	port := conn.LocalAddr().(*net.UDPAddr).Port
	var out []string
	ifaces, _ := net.Interfaces()
	seen := map[string]bool{}
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || isContainerBridge(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				add((&net.UDPAddr{IP: ipn.IP, Port: port}).String())
			}
		}
	}
	if pub != nil {
		add(pub.String())
	}
	return out
}

// isContainerBridge skips docker/podman/virtual bridge interfaces whose
// addresses are useless to announce (e.g. podman0 10.88.0.1).
func isContainerBridge(name string) bool {
	for _, p := range []string{"docker", "podman", "cni-", "veth", "br-"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
