package rnnoise

import (
	"math"
	"math/rand"
	"testing"
)

func TestDenoiseReducesNoise(t *testing.T) {
	s := New()
	defer s.Close()
	rng := rand.New(rand.NewSource(1))
	frame := make([]int16, FrameSize)
	var in, out float64
	for n := 0; n < 50; n++ {
		for i := range frame {
			frame[i] = int16(rng.NormFloat64() * 2000)
			in += float64(frame[i]) * float64(frame[i])
		}
		s.Process(frame)
		for _, v := range frame {
			out += float64(v) * float64(v)
		}
	}
	gain := 10 * math.Log10(out/in)
	t.Logf("noise gain %.1f dB", gain)
	if gain > -10 {
		t.Fatalf("expected white noise suppressed by >10 dB, got %.1f dB", gain)
	}
}
