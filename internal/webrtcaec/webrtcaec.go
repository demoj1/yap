// Package webrtcaec is the echo canceller Chrome used until 2018
// (modules/audio_processing/aec, BSD), vendored as plain C plus the
// three-band splitter it needs for 48 kHz. It finds the speaker-to-mic
// delay on its own, follows it when it drifts, and suppresses what the
// linear filter leaves behind.
package webrtcaec

/*
#cgo CFLAGS: -I${SRCDIR} -O2 -DNDEBUG
#cgo CXXFLAGS: -I${SRCDIR} -O2 -std=c++11 -DNDEBUG
#cgo linux windows LDFLAGS: -static-libstdc++ -static-libgcc
#include <stdint.h>
typedef struct yap_aec yap_aec;
yap_aec* yap_aec_create(int nlp);
void yap_aec_free(yap_aec* h);
void yap_aec_reset(yap_aec* h, int nlp);
void yap_aec_far(yap_aec* h, const int16_t* pcm);
int yap_aec_process(yap_aec* h, int16_t* pcm, int delay_ms);
int yap_aec_delay(yap_aec* h);
int yap_aec_status(yap_aec* h);
*/
import "C"

import "unsafe"

// Frame is the samples per call: 10 ms at 48 kHz.
const Frame = 480

// How hard the residual echo suppressor works.
const (
	Conservative = 0
	Moderate     = 1
	Aggressive   = 2
)

type Canceller struct{ h *C.yap_aec }

func New(nlp int) *Canceller { return &Canceller{C.yap_aec_create(C.int(nlp))} }

// Reset starts adapting from scratch with the given suppression level.
func (c *Canceller) Reset(nlp int) { C.yap_aec_reset(c.h, C.int(nlp)) }

// Far hands over the 10 ms the speakers are about to play.
func (c *Canceller) Far(pcm []int16) {
	if len(pcm) != Frame {
		panic("webrtcaec: far frame must be 480 samples")
	}
	C.yap_aec_far(c.h, (*C.int16_t)(unsafe.Pointer(&pcm[0])))
}

// Process takes the echo out of 10 ms of microphone, in place. delayMS is
// a guess of speakers-to-mic latency; the canceller refines it.
func (c *Canceller) Process(mic []int16, delayMS int) {
	if len(mic) != Frame {
		panic("webrtcaec: mic frame must be 480 samples")
	}
	C.yap_aec_process(c.h, (*C.int16_t)(unsafe.Pointer(&mic[0])), C.int(delayMS))
}

// Delay is the delay the canceller settled on, ms, or -1 while it has none.
func (c *Canceller) Delay() int { return int(C.yap_aec_delay(c.h)) }

// Echoing reports whether the canceller currently hears the speakers in the mic.
func (c *Canceller) Echoing() bool { return C.yap_aec_status(c.h) != 0 }

func (c *Canceller) Close() { C.yap_aec_free(c.h) }
