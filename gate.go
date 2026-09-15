package main

// gate is a noise gate for the outgoing stream: it opens instantly when the
// frame is loud enough to be speech and stays open for a short hold so word
// tails are not clipped, then closes. While closed the caller sends silence,
// so echo picked up from a speaker in the quiet gaps is never transmitted.
type gate struct {
	open int // frames left to stay open; >0 means pass audio
}

const (
	gateOpenPeak  = 900 // ~ -31 dBFS: above this a frame counts as speech
	gateClosePeak = 400 // ~ -38 dBFS: below this while open just runs down the hold
	gateHold      = 25  // frames (~500 ms) to keep passing after the last loud frame
)

// pass reports whether this frame should be sent as-is; when it returns
// false the caller should send silence.
func (g *gate) pass(pcm []int16) bool {
	var peak int
	for _, x := range pcm {
		if v := int(x); v > peak {
			peak = v
		} else if -v > peak {
			peak = -v
		}
	}
	switch {
	case peak >= gateOpenPeak:
		g.open = gateHold
	case g.open > 0 && peak < gateClosePeak: // sound in between holds steady rather than counting down
		g.open--
	}
	return g.open > 0
}
