# yap

One-to-one voice call from the terminal. No accounts, no servers, no UI.

```
yap listen              # prints yap://ip:port/secret — send it to a friend
yap join yap://...      # friend runs this
```

Opus 48 kHz / 96 kbps, RNNoise, ChaCha20-Poly1305 over raw UDP. Both ends
run the same binary. The listener must be reachable on the UDP port (public
IP or a port forward); the joiner can be behind any NAT.

## Build

Needs a C compiler and libopus (`pacman -S opus`, `brew install opus`,
msys2 `mingw-w64-x86_64-opus`).

```
go build
```

RNNoise (xiph, v0.2) is vendored in `internal/rnnoise`; its model is the
5.5 MB `weights_blob.bin` produced by `dump_weights_blob` from the upstream
repo and embedded into the binary.
