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
		clear(dst[n:])
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
		m = max(m, int32(s), -int32(s))
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

	aecMu   sync.Mutex     // guards aec against a knob change while onData runs it
	aec     *aec.Canceller // echo canceller, fed in onData
	aecOn   atomic.Bool    // echo cancellation enabled (it only runs while echo is heard)
	echo    echoTracker    // onData only
	echoing atomic.Bool    // the tracker currently hears the speakers in the mic
	echoLag atomic.Int32   // its delay estimate, ms, for the screen
	aecDB   atomic.Uint64  // float64 bits: smoothed dB the canceller took out of the mic lately
}

func sumSq(pcm []int16) float64 {
	var s float64
	for _, v := range pcm {
		s += float64(v) * float64(v)
	}
	return s
}

// setAEC applies the residual echo suppression knobs.
func (a *audio) setAEC(suppress, active int) {
	a.aecMu.Lock()
	a.aec.SetSuppress(suppress, active)
	a.aecMu.Unlock()
}

// Echo cancellation. The Speex canceller wants the sound the speakers play
// handed to it just before its echo shows up in the mic — a long tail that
// merely covers the delay adapts slowly and cancels poorly. So the played
// frames are kept for half a second and a tracker keeps estimating, from
// the correlation of the two loudness envelopes, how many frames later the
// mic hears them; the canceller gets the frame from that far back and a
// short tail. While the tracker hears no echo (headphones) it does not run.
const (
	aecTailMS  = 120  // about a third of a room's reverberation, per the Speex manual
	echoMaxLag = 50   // frames (500 ms) of playback history the echo is searched in
	echoHist   = 300  // frames (3 s) of envelopes an estimate is made over
	echoLead   = 2    // frames: the reference is fed this much ahead of the estimated echo
	echoEvery  = 100  // frames between estimates (1 s)
	echoOn     = 0.55 // envelope correlation that means the mic hears the speakers
	echoOff    = 0.30 // below this three estimates in a row, the echo is gone
)

type echoTracker struct {
	played [echoMaxLag + 1][frameSize / 2]int16 // ring of played frames, newest at head
	head   int
	ep, em [echoHist]float32 // loudness (dB) of played / mic frames, ring by frame number
	n      int               // frames seen
	lag    int               // frames from playback to its echo in the mic
	echo   bool              // the mic hears the speakers
	quiet  int               // estimates in a row below echoOff
}

func loudness(pcm []int16) float32 { return float32(10 * math.Log10(sumSq(pcm)/float64(len(pcm))+1)) }

// push records one callback's frames; true means an estimate is due.
func (e *echoTracker) push(spk, mic []int16) bool {
	e.head = (e.head + 1) % len(e.played)
	copy(e.played[e.head][:], spk)
	e.ep[e.n%echoHist], e.em[e.n%echoHist] = loudness(spk), loudness(mic)
	e.n++
	return e.n >= echoHist && e.n%echoEvery == 0
}

// estimate correlates the mic envelope with the played one at every lag and
// decides whether there is echo and how late it is. True means the lag
// changed and the canceller must adapt from scratch.
func (e *echoTracker) estimate() bool {
	const n = echoHist - echoMaxLag // frames compared per lag
	bestLag, best := 0, 0.0
	for lag := 0; lag <= echoMaxLag; lag++ {
		var sx, sy, sxx, syy, sxy float64
		for t := 0; t < n; t++ { // newest first; the played frame lag earlier
			x, y := float64(e.ep[(e.n-1-t-lag)%echoHist]), float64(e.em[(e.n-1-t)%echoHist])
			sx, sy, sxx, syy, sxy = sx+x, sy+y, sxx+x*x, syy+y*y, sxy+x*y
		}
		if den := math.Sqrt((sxx - sx*sx/n) * (syy - sy*sy/n)); den > 0 {
			if r := (sxy - sx*sy/n) / den; r > best {
				best, bestLag = r, lag
			}
		}
	}
	switch {
	case best >= echoOn:
		e.quiet = 0
		changed := !e.echo || bestLag != e.lag
		e.echo, e.lag = true, bestLag
		return changed
	case e.echo && best < echoOff:
		if e.quiet++; e.quiet >= 3 {
			e.echo = false
		}
	}
	return false
}

// reference is the played frame the mic is about to echo, fed a little early.
func (e *echoTracker) reference() []int16 {
	back := max(0, e.lag-echoLead)
	return e.played[(e.head-back+len(e.played))%len(e.played)][:]
}

// openAudio starts a full-duplex 48 kHz mono device. Captured 20 ms frames
// arrive on a.frames; playback is fed through a.play.
func openAudio(micName, outName string) (*audio, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	a := &audio{ctx: ctx, frames: make(chan []int16, 8), play: newPCMQueue(), mic: micName, out: outName}
	a.aec = aec.New(frameSize/2, sampleRate*aecTailMS/1000, sampleRate) // 10 ms frames
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
		if a.echo.push(spk, mic) && a.echo.estimate() {
			a.aecMu.Lock()
			a.aec.Reset() // the delay moved: adapt from scratch
			a.aecMu.Unlock()
		}
		a.echoing.Store(a.echo.echo)
		a.echoLag.Store(int32(a.echo.lag * frameMS / 2))
		if a.echo.echo {
			before := sumSq(mic)
			a.aecMu.Lock()
			a.aec.Process(mic, a.echo.reference()) // remove what the speakers played from the mic
			a.aecMu.Unlock()
			if before > 1e6 { // only meaningful when there was something to cancel
				db := 10 * math.Log10(before/(sumSq(mic)+1))
				prev := math.Float64frombits(a.aecDB.Load())
				a.aecDB.Store(math.Float64bits(prev + (db-prev)*0.1))
			}
		}
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
