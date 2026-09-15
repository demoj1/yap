package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const scheme = "yap://"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// link is the shared secret behind yap://<secret>. Everything else — the
// rendezvous topic and both keys — is derived from it.
type link struct {
	secret []byte
}

func newLink() link {
	return link{randBytes(16)}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func parseLink(s string) (link, error) {
	enc, ok := strings.CutPrefix(strings.TrimSpace(s), scheme)
	if !ok {
		return link{}, fmt.Errorf("link must start with %s", scheme)
	}
	raw, err := b32.DecodeString(strings.ToUpper(enc))
	if err != nil || len(raw) != 16 {
		return link{}, fmt.Errorf("bad link")
	}
	return link{raw}, nil
}

// loadOrCreateLink keeps the listener's link across restarts in the user
// config dir, so a friend can keep calling the same address.
func loadOrCreateLink(rotate bool) link {
	path := filepath.Join(configDir(), "link")
	if !rotate {
		if raw, err := os.ReadFile(path); err == nil {
			l, err := parseLink(string(raw))
			if err != nil {
				panic(path + ": " + err.Error())
			}
			return l
		}
	}
	l := newLink()
	must(os.WriteFile(path, []byte(l.String()+"\n"), 0o600))
	return l
}

func (l link) String() string {
	return scheme + strings.ToLower(b32.EncodeToString(l.secret))
}

func (l link) derive(label string, extra ...[]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(label))
	h.Write(l.secret)
	for _, e := range extra {
		h.Write(e)
	}
	return [32]byte(h.Sum(nil))
}

func (l link) topic() string {
	k := l.derive("topic")
	return "yap-" + strings.ToLower(b32.EncodeToString(k[:12]))
}

func (l link) sigKey() [32]byte { return l.derive("sig") }

// mediaKey is fresh per call: both sides contribute a nonce through the
// rendezvous, so packet counters restarting at 0 never reuse a keystream.
func (l link) mediaKey(offerNonce, answerNonce []byte) [32]byte {
	return l.derive("media", offerNonce, answerNonce)
}
