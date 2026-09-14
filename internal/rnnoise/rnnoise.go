// Package rnnoise wraps xiph RNNoise v0.2 (BSD, see COPYING). Model weights
// are loaded at runtime from the embedded blob instead of a 29 MB C array.
package rnnoise

/*
#cgo CFLAGS: -O2 -Wno-cpp -DUSE_WEIGHTS_FILE -DRNNOISE_BUILD -I${SRCDIR}
#cgo LDFLAGS: -lm
#include <stdlib.h>
#include "rnnoise.h"
*/
import "C"

import (
	_ "embed"
	"unsafe"
)

//go:embed weights_blob.bin
var weights []byte

const FrameSize = 480

var model = func() *C.RNNModel {
	m := C.rnnoise_model_from_buffer(unsafe.Pointer(&weights[0]), C.int(len(weights)))
	if m == nil {
		panic("rnnoise: bad weights blob")
	}
	return m
}()

type State struct {
	st  *C.DenoiseState
	buf []C.float
}

func New() *State {
	return &State{st: C.rnnoise_create(model), buf: make([]C.float, FrameSize)}
}

// Process denoises exactly FrameSize samples in place, returns voice probability.
func (s *State) Process(pcm []int16) float32 {
	if len(pcm) != FrameSize {
		panic("rnnoise: frame must be 480 samples")
	}
	for i, v := range pcm {
		s.buf[i] = C.float(v)
	}
	vad := C.rnnoise_process_frame(s.st, &s.buf[0], &s.buf[0])
	for i, v := range s.buf {
		switch {
		case v > 32767:
			pcm[i] = 32767
		case v < -32768:
			pcm[i] = -32768
		default:
			pcm[i] = int16(v)
		}
	}
	return float32(vad)
}

func (s *State) Close() { C.rnnoise_destroy(s.st) }
