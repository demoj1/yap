package main

import "sync/atomic"

var bitrates = []int{12, 16, 24, 32, 48, 64, 96, 128, 160} // kbps

// controls are the live knobs the UI turns for our own outgoing stream;
// loops read them every frame. Per-peer volume lives on the peer.
type controls struct {
	bitrate atomic.Int32 // kbps
	muted   atomic.Bool
	denoise atomic.Bool
}

func (c *controls) stepBitrate(dir int) {
	cur := int(c.bitrate.Load())
	for i, b := range bitrates {
		if b == cur {
			c.bitrate.Store(int32(bitrates[max(0, min(len(bitrates)-1, i+dir))]))
			return
		}
	}
}
