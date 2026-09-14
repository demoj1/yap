package main

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// pcmQueue is the playback buffer between the network decoder and the sound card.
type pcmQueue struct {
	mu       sync.Mutex
	buf      []int16
	need     chan struct{}
	underrun atomic.Uint64 // samples of silence inserted
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
	low := len(q.buf) < playTarget*frameSize
	q.mu.Unlock()
	if n < len(dst) {
		q.underrun.Add(uint64(len(dst) - n))
		for i := n; i < len(dst); i++ {
			dst[i] = 0
		}
	}
	if low {
		select {
		case q.need <- struct{}{}:
		default:
		}
	}
}

// peak tracks the loudest sample since the last take(), in dBFS.
type peak struct{ v atomic.Int32 }

func (p *peak) observe(pcm []int16) {
	var m int32
	for _, s := range pcm {
		if a := int32(s); a > m {
			m = a
		} else if -a > m {
			m = -a
		}
	}
	for {
		cur := p.v.Load()
		if m <= cur || p.v.CompareAndSwap(cur, m) {
			return
		}
	}
}

func (p *peak) take() float64 {
	m := p.v.Swap(0)
	if m == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(float64(m)/32768)
}

type audio struct {
	ctx    *malgo.AllocatedContext
	dev    *malgo.Device
	acc    []int16
	frames chan []int16
	play   *pcmQueue

	period           atomic.Uint32 // frames per callback as the backend actually delivers them
	capDrop          atomic.Uint64
	micPeak, spkPeak peak
}

// openAudio starts a full-duplex 48 kHz mono device. Captured 20 ms frames
// arrive on a.frames; playback is fed through a.play.
func openAudio(micName, outName string) (*audio, error) {
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
	capID := deviceID(ctx.Context, malgo.Capture, micName)
	playID := deviceID(ctx.Context, malgo.Playback, outName)
	var pin runtime.Pinner
	if capID != nil {
		pin.Pin(capID)
		cfg.Capture.DeviceID = unsafe.Pointer(capID)
	}
	if playID != nil {
		pin.Pin(playID)
		cfg.Playback.DeviceID = unsafe.Pointer(playID)
	}
	defer pin.Unpin() // InitDevice copies the IDs into the device

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
	a.period.Store(count)
	spk := s16(out, count)
	a.play.pull(spk)
	a.spkPeak.observe(spk)
	mic := s16(in, count)
	a.micPeak.observe(mic)
	a.acc = append(a.acc, mic...)
	for len(a.acc) >= frameSize {
		f := make([]int16, frameSize)
		copy(f, a.acc)
		a.acc = a.acc[frameSize:]
		select {
		case a.frames <- f:
		default:
			a.capDrop.Add(1)
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
