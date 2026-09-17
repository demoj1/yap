package main

import (
	"math"
	"math/rand"
	"testing"

	"github.com/demoj1/yap/internal/webrtcaec"
)

// echoRun plays 30 s of bursty noise through the canceller with an echo
// path that starts delayMS late, drifts by ppm (the mic clock slower than
// the speakers'), and jumps by jumpMS halfway. Returns echo reduction in
// dB over the last 5 s.
func echoRun(t *testing.T, delayMS int, ppm float64, jumpMS int) float64 {
	const frames = 3000 // 30 s of 10 ms
	rng := rand.New(rand.NewSource(11))
	far := make([]float64, frames*webrtcaec.Frame)
	for i := range far {
		if i/(webrtcaec.Frame*24)%3 != 0 { // 240 ms bursts, 120 ms pauses
			far[i] = rng.NormFloat64() * 5000
		}
	}
	echoAt := func(i int) float64 { // what the mic hears at sample i
		d := delayMS
		if i >= len(far)/2 {
			d += jumpMS
		}
		pos := float64(i)*(1-ppm*1e-6) - float64(d*sampleRate/1000)
		k := int(math.Floor(pos))
		if k < 0 || k+1 >= len(far) {
			return 0
		}
		fr := pos - float64(k)
		return (far[k]*(1-fr) + far[k+1]*fr) * 0.5
	}
	c := webrtcaec.New(webrtcaec.Moderate)
	defer c.Close()
	spk, mic := make([]int16, webrtcaec.Frame), make([]int16, webrtcaec.Frame)
	var in, out float64
	for f := 0; f < frames; f++ {
		for i := range spk {
			spk[i] = int16(far[f*webrtcaec.Frame+i])
			mic[i] = int16(echoAt(f*webrtcaec.Frame+i) + rng.NormFloat64()*100)
		}
		measure := f >= frames-500
		if measure {
			in += sumSq(mic)
		}
		c.Far(spk)
		c.Process(mic, aecDelayGuessMS)
		if measure {
			out += sumSq(mic)
		}
	}
	erle := 10 * math.Log10(in/out)
	t.Logf("delay %d ms, drift %.0f ppm, jump %d ms: %.1f dB, echoing=%v, delay seen %d ms", delayMS, ppm, jumpMS, erle, c.Echoing(), c.Delay())
	return erle
}

func TestWebRTCCancelsSteadyEcho(t *testing.T) {
	if e := echoRun(t, 200, 0, 0); e < 20 {
		t.Fatalf("only %.1f dB", e)
	}
}

// A USB mic and onboard speakers tick at different rates; the canceller
// must keep up with the echo path sliding under it.
func TestWebRTCRidesClockDrift(t *testing.T) {
	if e := echoRun(t, 200, 300, 0); e < 20 {
		t.Fatalf("only %.1f dB with drift", e)
	}
}

func TestWebRTCFollowsDelayJump(t *testing.T) {
	if e := echoRun(t, 200, 0, 100); e < 20 {
		t.Fatalf("only %.1f dB after the delay moved", e)
	}
}

func TestWebRTCLeavesHeadphonesAlone(t *testing.T) {
	c := webrtcaec.New(webrtcaec.Moderate)
	defer c.Close()
	rng := rand.New(rand.NewSource(5))
	spk, mic := make([]int16, webrtcaec.Frame), make([]int16, webrtcaec.Frame)
	var in, out float64
	for f := 0; f < 1000; f++ {
		for i := range spk {
			spk[i] = int16(rng.NormFloat64() * 5000)
			mic[i] = int16(rng.NormFloat64() * 2000) // our own voice, no echo
		}
		measure := f >= 500
		if measure {
			in += sumSq(mic)
		}
		c.Far(spk)
		c.Process(mic, aecDelayGuessMS)
		if measure {
			out += sumSq(mic)
		}
	}
	t.Logf("headphones: echoing=%v delay %d", c.Echoing(), c.Delay())
	if loss := 10 * math.Log10(in/out); loss > 3 {
		t.Fatalf("no echo path, yet the voice lost %.1f dB", loss)
	}
}
