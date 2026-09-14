package main

import (
	"sync"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// pcmQueue is the playback buffer between the network decoder and the sound card.
type pcmQueue struct {
	mu   sync.Mutex
	buf  []int16
	need chan struct{}
}

func newPCMQueue() *pcmQueue {
	return &pcmQueue{need: make(chan struct{}, 1)}
}

func (q *pcmQueue) push(pcm []int16) {
	q.mu.Lock()
	q.buf = append(q.buf, pcm...)
	q.mu.Unlock()
}

func (q *pcmQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.buf)
}

// pull fills dst from the queue, zero-padding on underrun, and wakes the
// decoder when less than one frame is left.
func (q *pcmQueue) pull(dst []int16) {
	q.mu.Lock()
	n := copy(dst, q.buf)
	q.buf = q.buf[n:]
	low := len(q.buf) < frameSize
	q.mu.Unlock()
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
	if low {
		select {
		case q.need <- struct{}{}:
		default:
		}
	}
}

type audio struct {
	ctx    *malgo.AllocatedContext
	dev    *malgo.Device
	acc    []int16
	frames chan []int16
	play   *pcmQueue
}

// openAudio starts a full-duplex 48 kHz mono device. Captured 20 ms frames
// arrive on a.frames; playback is fed through a.play.
func openAudio() (*audio, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	a := &audio{ctx: ctx, frames: make(chan []int16, 8), play: newPCMQueue()}

	cfg := malgo.DefaultDeviceConfig(malgo.Duplex)
	cfg.SampleRate = sampleRate
	cfg.PeriodSizeInFrames = frameSize / 2
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = 1
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = 1

	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{Data: a.onData})
	if err != nil {
		ctx.Uninit()
		ctx.Free()
		return nil, err
	}
	a.dev = dev
	if err := dev.Start(); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

func (a *audio) onData(out, in []byte, count uint32) {
	a.play.pull(s16(out, count))
	a.acc = append(a.acc, s16(in, count)...)
	for len(a.acc) >= frameSize {
		f := make([]int16, frameSize)
		copy(f, a.acc)
		a.acc = a.acc[frameSize:]
		select {
		case a.frames <- f:
		default:
		}
	}
}

func s16(b []byte, n uint32) []int16 {
	return unsafe.Slice((*int16)(unsafe.Pointer(&b[0])), n)
}

func (a *audio) Close() {
	a.dev.Uninit()
	a.ctx.Uninit()
	a.ctx.Free()
}
