package main

import (
	"sync/atomic"
	"time"
)

// pttHold is how long one space keypress keeps the mic open in push-to-talk.
// A held key auto-repeats faster than this, so holding space talks and
// letting go closes the mic within this window.
const pttHold = 600 * time.Millisecond

// duckHold is how long after the last loud speaker frame the mic stays down.
const duckHold = 300 * time.Millisecond

var bitrates = []int{12, 16, 24, 32, 48, 64, 96, 128, 160} // kbps

// controls are the live knobs the UI turns for our own outgoing stream;
// loops read them every frame. Per-peer volume lives on the peer.
type controls struct {
	bitrate atomic.Int32 // kbps
	muted   atomic.Bool
	denoise atomic.Bool
	gate    atomic.Bool  // noise gate on the send path: stay silent until you actually speak
	aec     atomic.Bool  // acoustic echo cancellation (SpeexDSP)
	agc     atomic.Bool  // automatic gain control: normalize outgoing loudness
	micGain atomic.Int32 // percent applied to the mic after AGC; 100 is as captured

	sounds    atomic.Bool  // chimes for people coming and going and for chat messages
	ptt       atomic.Bool  // push-to-talk: silent unless space is being held
	duck      atomic.Bool  // while the speakers play a voice, the mic is held down 30 dB: no echo, no interrupting
	talkUntil atomic.Int64 // unix nanos until which the last space press keeps the mic open
}

// silenced reports whether the outgoing stream must carry silence right now:
// muted, or push-to-talk without space held.
func (c *controls) silenced() bool {
	return c.muted.Load() || (c.ptt.Load() && !c.talking())
}

// talking reports whether space is currently held in push-to-talk.
func (c *controls) talking() bool {
	return time.Now().UnixNano() <= c.talkUntil.Load()
}

func (c *controls) pressTalk() { c.talkUntil.Store(time.Now().Add(pttHold).UnixNano()) }

func (c *controls) stepBitrate(dir int) {
	cur := int(c.bitrate.Load())
	for i, b := range bitrates {
		if b == cur {
			c.bitrate.Store(int32(bitrates[max(0, min(len(bitrates)-1, i+dir))]))
			return
		}
	}
}
