# yap

One-to-one voice call from the terminal. No accounts, no servers, no UI.

```
yap listen              # prints yap://ip:port/secret — send it to a friend
yap join yap://...      # friend runs this
```

Opus 48 kHz / 96 kbps, RNNoise, ChaCha20-Poly1305 over raw UDP, peer to peer.
Both ends run the same binary. NAT traversal: each side learns its public
address via STUN and the two swap encrypted address lists through a random
ntfy.sh topic derived from the link, then UDP hole-punch. Works through
cone NATs (most home routers); a symmetric NAT on either side will fail —
there is no relay.

## Build

Needs a C compiler and libopus (`pacman -S opus`, `brew install opus`,
msys2 `mingw-w64-x86_64-opus`).

```
go build -tags nolibopusfile
```

macOS binary: GitHub Actions (`release.yml`) builds a universal arm64+x86_64
`yap-macos` with libopus linked statically. Download it with `curl` (a browser
download gets quarantined) and `chmod +x`.

RNNoise (xiph, v0.2) is vendored in `internal/rnnoise`; its model is the
5.5 MB `weights_blob.bin` produced by `dump_weights_blob` from the upstream
repo and embedded into the binary.
