package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
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
	s := make([]byte, 16)
	if _, err := rand.Read(s); err != nil {
		panic(err)
	}
	return link{s}
}

func parseLink(s string) (link, error) {
	enc, ok := strings.CutPrefix(s, scheme)
	if !ok {
		return link{}, fmt.Errorf("link must start with %s", scheme)
	}
	raw, err := b32.DecodeString(strings.ToUpper(enc))
	if err != nil || len(raw) != 16 {
		return link{}, fmt.Errorf("bad link")
	}
	return link{raw}, nil
}

func (l link) String() string {
	return scheme + strings.ToLower(b32.EncodeToString(l.secret))
}

func (l link) derive(label string) [32]byte {
	return sha256.Sum256(append([]byte(label), l.secret...))
}

func (l link) topic() string {
	k := l.derive("topic")
	return "yap-" + strings.ToLower(b32.EncodeToString(k[:12]))
}

func (l link) mediaKey() [32]byte { return l.derive("media") }
func (l link) sigKey() [32]byte   { return l.derive("sig") }
