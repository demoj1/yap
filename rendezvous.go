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

	"golang.org/x/crypto/chacha20poly1305"
)

const ntfy = "https://ntfy.sh/"

// hello is what each side publishes to the rendezvous topic: its role and
// every address it might be reachable at (LAN + STUN-mapped public).
type hello struct {
	Role  uint32   `json:"role"`
	Addrs []string `json:"addrs"`
}

// room is a public ntfy.sh topic used only to swap encrypted hellos.
type room struct {
	topic string
	aead  cipher.AEAD
}

func newRoom(l link) *room {
	key := l.sigKey()
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		panic(err)
	}
	return &room{topic: l.topic(), aead: aead}
}

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
	resp, err := http.Post(ntfy+r.topic, "text/plain", strings.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("ntfy: %s", resp.Status)
	}
	return nil
}

// listen streams hellos from the other side. It returns once the
// subscription is open, so a say() after it cannot be missed.
func (r *room) listen(ctx context.Context, want uint32) (<-chan hello, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", ntfy+r.topic+"/json", nil)
	if err != nil {
		panic(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(resp.Body)
	var ev struct {
		Event   string `json:"event"`
		Message string `json:"message"`
	}
	for sc.Scan() {
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Event == "open" {
			break
		}
	}
	if sc.Err() != nil {
		return nil, sc.Err()
	}
	out := make(chan hello)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		for sc.Scan() {
			if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "message" {
				continue
			}
			if h, ok := r.open(ev.Message); ok && h.Role == want {
				out <- h
			}
		}
	}()
	return out, nil
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
// the STUN-mapped public address.
func candidates(conn *net.UDPConn) []string {
	port := conn.LocalAddr().(*net.UDPAddr).Port
	var out []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, (&net.UDPAddr{IP: ipn.IP, Port: port}).String())
			}
		}
	}
	if pub, err := publicAddr(conn); err == nil {
		out = append(out, pub.String())
	}
	return out
}
