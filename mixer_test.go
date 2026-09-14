package main

import (
	"math"
	"testing"
)

func sine(amp float64, hz float64) []int16 {
	f := make([]int16, frameSize)
	for i := range f {
		f[i] = int16(amp * math.Sin(2*math.Pi*hz*float64(i)/sampleRate))
	}
	return f
}

func peakOf(f []int16) int {
	var p int
	for _, x := range f {
		if v := int(x); v > p {
			p = v
		} else if -v > p {
			p = -v
		}
	}
	return p
}

func TestQuietMixPassesUntouched(t *testing.T) {
	mix := make([]int32, frameSize)
	out := make([]int16, frameSize)
	in := sine(8000, 440)
	mixInto(mix, in, 100)
	var l limiter
	l.apply(mix, out)
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("sample %d changed: %d -> %d", i, in[i], out[i])
		}
	}
	if l.gain != 1 {
		t.Fatalf("gain must stay 1, got %f", l.gain)
	}
}

func TestVolumeScales(t *testing.T) {
	mix := make([]int32, frameSize)
	out := make([]int16, frameSize)
	mixInto(mix, sine(10000, 440), 50)
	var l limiter
	l.apply(mix, out)
	if p := peakOf(out); p < 4900 || p > 5100 {
		t.Fatalf("50%% of a 10000 peak should be ~5000, got %d", p)
	}
}

func TestThreeLoudVoicesDoNotClip(t *testing.T) {
	mix := make([]int32, frameSize)
	out := make([]int16, frameSize)
	for _, hz := range []float64{200, 330, 470} {
		mixInto(mix, sine(30000, hz), 100)
	}
	var l limiter
	l.apply(mix, out)
	if p := peakOf(out); p > 32767 || p < 30000 {
		t.Fatalf("limited peak should sit just under full scale, got %d", p)
	}
	if l.gain >= 1 {
		t.Fatalf("gain must have dropped, got %f", l.gain)
	}

	// After the loud burst, quiet frames bring the gain back toward 1.
	quiet := sine(1000, 440)
	for i := 0; i < 60; i++ {
		clear(mix)
		mixInto(mix, quiet, 100)
		l.apply(mix, out)
	}
	if l.gain < 0.95 {
		t.Fatalf("gain should have released, still %f", l.gain)
	}
}
