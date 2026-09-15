package aec

import (
	"math"
	"math/rand"
	"testing"
)

// A far-end signal leaks into the mic delayed and attenuated (an echo path).
// After the filter adapts, the canceller should suppress that echo well.
func TestCancelsEcho(t *testing.T) {
	const (
		frame = 480
		tail  = 4800 // 100 ms @ 48k
		rate  = 48000
		delay = 120 // samples of echo path delay
		atten = 0.5 // echo is half the far-end amplitude
	)
	c := New(frame, tail, rate)
	defer c.Close()
	rng := rand.New(rand.NewSource(1))

	far := make([][]int16, 400) // speaker signal per frame
	for f := range far {
		far[f] = make([]int16, frame)
		for i := range far[f] {
			far[f][i] = int16(rng.NormFloat64() * 6000)
		}
	}
	// Build the mic as the delayed, attenuated far-end (pure echo, no near speech).
	flat := make([]int16, 0, len(far)*frame)
	for _, fr := range far {
		flat = append(flat, fr...)
	}
	var echoEnergy, outEnergy float64
	for f := 0; f < len(far); f++ {
		mic := make([]int16, frame)
		for i := range mic {
			j := f*frame + i - delay
			if j >= 0 {
				mic[i] = int16(float64(flat[j]) * atten)
			}
		}
		play := make([]int16, frame)
		copy(play, far[f])
		if f > 300 { // measure after the filter has adapted
			for _, v := range mic {
				echoEnergy += float64(v) * float64(v)
			}
		}
		c.Process(mic, play)
		if f > 300 {
			for _, v := range mic {
				outEnergy += float64(v) * float64(v)
			}
		}
	}
	erle := 10 * math.Log10(echoEnergy/outEnergy)
	t.Logf("echo reduction %.1f dB", erle)
	if erle < 12 {
		t.Fatalf("expected >12 dB echo suppression once adapted, got %.1f dB", erle)
	}
}
