// Package aec wraps the SpeexDSP acoustic echo canceller (xiph, BSD — see
// COPYING). It removes, from the microphone, the sound the speakers are
// playing, so a participant on speakers no longer echoes everyone back.
package aec

/*
#cgo CFLAGS: -O2 -DHAVE_CONFIG_H -I${SRCDIR} -I${SRCDIR}/speex -Wno-unused-but-set-variable
#cgo LDFLAGS: -lm
#include "speex/speex_echo.h"
#include "speex/speex_preprocess.h"
#include <stdlib.h>
*/
import "C"

import (
	"runtime"
	"unsafe"
)

// Canceller cancels echo for one fixed frame size at one sample rate. Feed it
// each captured frame together with the frame that is being played back at the
// same moment; it returns the microphone with the speaker signal removed.
type Canceller struct {
	echo  *C.SpeexEchoState
	pre   *C.SpeexPreprocessState
	frame int
	out   []C.spx_int16_t
	rec   []C.spx_int16_t
	play  []C.spx_int16_t
}

// New builds a canceller. frame is samples per call, tail the adaptive filter
// length in samples (echo path length it can cover, e.g. 100 ms).
func New(frame, tail, sampleRate int) *Canceller {
	c := &Canceller{
		echo:  C.speex_echo_state_init(C.int(frame), C.int(tail)),
		frame: frame,
		out:   make([]C.spx_int16_t, frame),
		rec:   make([]C.spx_int16_t, frame),
		play:  make([]C.spx_int16_t, frame),
	}
	rate := C.int(sampleRate)
	C.speex_echo_ctl(c.echo, C.SPEEX_ECHO_SET_SAMPLING_RATE, unsafe.Pointer(&rate))
	c.pre = C.speex_preprocess_state_init(C.int(frame), rate)
	C.speex_preprocess_ctl(c.pre, C.SPEEX_PREPROCESS_SET_ECHO_STATE, unsafe.Pointer(c.echo))
	return c
}

// Process removes play (the speaker frame) from rec (the mic frame) in place,
// writing the cleaned microphone back into rec. Both slices must be frame long.
func (c *Canceller) Process(rec, play []int16) {
	if len(rec) != c.frame || len(play) != c.frame {
		panic("aec: wrong frame size")
	}
	for i := 0; i < c.frame; i++ {
		c.rec[i] = C.spx_int16_t(rec[i])
		c.play[i] = C.spx_int16_t(play[i])
	}
	C.speex_echo_cancellation(c.echo, &c.rec[0], &c.play[0], &c.out[0])
	C.speex_preprocess_run(c.pre, &c.out[0])
	for i := 0; i < c.frame; i++ {
		rec[i] = int16(c.out[i])
	}
	runtime.KeepAlive(c)
}

func (c *Canceller) Close() {
	C.speex_preprocess_state_destroy(c.pre)
	C.speex_echo_state_destroy(c.echo)
}
