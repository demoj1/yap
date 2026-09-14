package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net"
	"strings"
)

const scheme = "yap://"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

type link struct {
	addr *net.UDPAddr
	key  [32]byte // chacha20-poly1305 key derived from the 16-byte secret in the URL
}

func newSecret() string {
	var s [16]byte
	if _, err := rand.Read(s[:]); err != nil {
		panic(err)
	}
	return strings.ToLower(b32.EncodeToString(s[:]))
}

func formatLink(host string, port int, secret string) string {
	return fmt.Sprintf("%s%s/%s", scheme, net.JoinHostPort(host, fmt.Sprint(port)), secret)
}

func parseLink(s string) (*link, error) {
	rest, ok := strings.CutPrefix(s, scheme)
	if !ok {
		return nil, fmt.Errorf("link must start with %s", scheme)
	}
	hostport, secret, ok := strings.Cut(rest, "/")
	if !ok {
		return nil, fmt.Errorf("link has no secret part")
	}
	addr, err := net.ResolveUDPAddr("udp", hostport)
	if err != nil {
		return nil, err
	}
	return &link{addr: addr, key: keyFromSecret(secret)}, nil
}

func keyFromSecret(secret string) [32]byte {
	raw, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil || len(raw) != 16 {
		panic("bad secret in link")
	}
	return sha256.Sum256(raw)
}
