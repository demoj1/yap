package main

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/demoj1/yap/internal/aec"
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

	mu       sync.Mutex // guards a device swap against Close
	mic, out string     // current device names, "" = system default

	aecMu   sync.Mutex     // guards aec against a rebuild while onData runs it
	aecTail int            // samples the current canceller was built with
	aec     *aec.Canceller // echo canceller, fed in onData; nil until built
	aecOn   atomic.Bool    // whether to run it
}

// setAEC applies the echo canceller knobs: a new tail (ms) rebuilds the
// adaptive filter (it re-converges in a second or two), suppression levels
// take effect at once.
func (a *audio) setAEC(tailMS, suppress, active int) {
	a.aecMu.Lock()
	defer a.aecMu.Unlock()
	if tail := sampleRate * tailMS / 1000; a.aec == nil || tail != a.aecTail {
		if a.aec != nil {
			a.aec.Close()
		}
		a.aec, a.aecTail = aec.New(frameSize/2, tail, sampleRate), tail
	}
	a.aec.SetSuppress(suppress, active)
}

// openAudio starts a full-duplex 48 kHz mono device. Captured 20 ms frames
// arrive on a.frames; playback is fed through a.play.
func openAudio(micName, outName string) (*audio, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	a := &audio{ctx: ctx, frames: make(chan []int16, 8), play: newPCMQueue(), mic: micName, out: outName}
	// 10 ms frames, 300 ms echo tail by default: the echo of what we hand to
	// the device now comes back in the mic after output + input latency plus
	// the room — 30–60 ms on a wired card, 150–250 ms on Bluetooth headsets.
	a.setAEC(300, -60, -30)
	if err := a.startDevice(); err != nil {
		ctx.Uninit()
		ctx.Free()
		return nil, err
	}
	return a, nil
}

// startDevice opens the duplex device for the current a.mic / a.out. The
// caller holds a.mu (or is the constructor).
func (a *audio) startDevice() error {
	cfg := malgo.DefaultDeviceConfig(malgo.Duplex)
	cfg.SampleRate = sampleRate
	cfg.PeriodSizeInFrames = frameSize / 2
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = 1
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = 1
	capID := deviceID(a.ctx.Context, malgo.Capture, a.mic)
	playID := deviceID(a.ctx.Context, malgo.Playback, a.out)
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

	dev, err := malgo.InitDevice(a.ctx.Context, cfg, malgo.DeviceCallbacks{Data: a.onData})
	if err != nil {
		return err
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return err
	}
	a.dev = dev
	return nil
}

// reopen switches to different devices without dropping the call: the frames
// and playback queue live on the audio struct, only the malgo device is
// swapped. On failure it restores the previous device. Returns the names now
// in effect.
func (a *audio) reopen(mic, out string) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prevMic, prevOut, prevDev := a.mic, a.out, a.dev
	a.dev.Uninit() // blocks until callbacks stop
	a.mic, a.out = mic, out
	if err := a.startDevice(); err != nil {
		a.mic, a.out, a.dev = prevMic, prevOut, prevDev
		if e2 := a.startDevice(); e2 != nil {
			return prevMic, prevOut, e2
		}
		return prevMic, prevOut, err
	}
	return mic, out, nil
}

func (a *audio) onData(out, in []byte, count uint32) {
	a.period.Store(count)
	spk := s16(out, count)
	a.play.pull(spk)
	a.spkPeak.observe(spk)
	mic := s16(in, count)
	if a.aecOn.Load() && int(count) == frameSize/2 {
		a.aecMu.Lock()
		a.aec.Process(mic, spk) // remove what the speakers are playing from the mic
		a.aecMu.Unlock()
	}
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dev != nil {
		a.dev.Uninit()
	}
	a.ctx.Uninit()
	a.ctx.Free()
	a.aecMu.Lock()
	a.aec.Close()
	a.aecMu.Unlock()
}

// describe names the devices actually in use, resolving "" to the system
// default's name, for the startup log.
func (a *audio) describe() string {
	name := func(kind malgo.DeviceType, chosen string) string {
		if chosen != "" {
			return chosen
		}
		devs, err := a.ctx.Devices(kind)
		if err != nil {
			return "default"
		}
		for i := range devs {
			if devs[i].IsDefault != 0 {
				return devs[i].Name() + " (default)"
			}
		}
		return "default"
	}
	return fmt.Sprintf("mic %q · out %q · %d Hz · period %d", name(malgo.Capture, a.mic), name(malgo.Playback, a.out), sampleRate, frameSize/2)
}
