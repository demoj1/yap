# yap

One-to-one voice call from the terminal. No accounts, no servers, no UI.

```
yap listen              # prints yap://<secret> — send it to a friend; the link survives restarts
yap join yap://...      # friend runs this
```

Both ends get a TUI: level meters, `↑/↓` friend volume, `m` mute, `d` RNNoise
on/off, `+/-` Opus bitrate (12–160 kbps, live), `q` quit. `-name` sets what the
friend sees, `-plain` gives logs instead of the TUI. A dropped call goes back
to waiting/redialing by itself. `yap devices` lists microphones and speakers; `-mic` / `-out` pick one by
name or unique prefix and are remembered, and `i` / `o` in the TUI switch them
live mid-call. Per-friend volume, bitrate and the
denoise toggle persist in `<config dir>/yap/settings.json`. Everything is also logged to
`<config dir>/yap/yap.log` (`~/.config/yap` on Linux, `~/Library/Application Support/yap`
on macOS) — send that when something goes wrong.

Opus 48 kHz / 96 kbps, RNNoise, ChaCha20-Poly1305 over raw UDP, peer to peer.
Both ends run the same binary. NAT traversal: each side learns its public
address via STUN and the two swap encrypted address lists through a random
ntfy.sh topic derived from the link, then UDP hole-punch. Works through
cone NATs (most home routers); a symmetric NAT on either side will fail —
there is no relay.

## Download

Each tagged release has prebuilt binaries: `yap-macos` (universal) and
`yap-linux-amd64`. Grab one, `chmod +x`, run. On macOS a browser download is
quarantined — fetch it with `curl -L` or clear it with
`xattr -d com.apple.quarantine yap-macos`.

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
