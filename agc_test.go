package main

import (
	"math"
	"testing"
)

func rmsOf(pcm []int16) float64 {
	var s float64
	for _, x := range pcm {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s / float64(len(pcm)))
}

func TestAGCBringsQuietUp(t *testing.T) {
	var a agc
	// A quiet talker: RMS well below target.
	quiet := sine(1500, 300) // peak 1500 → rms ~1060
	for i := 0; i < 400; i++ {
		f := append([]int16(nil), quiet...)
		a.process(f)
		if i == 399 {
			got := rmsOf(f)
			if got < agcTargetRMS*0.7 {
				t.Fatalf("quiet speech not raised enough: rms %.0f target %d", got, agcTargetRMS)
			}
		}
	}
	if a.gain < 2 {
		t.Fatalf("expected gain to climb for quiet input, got %.2f", a.gain)
	}
}

func TestAGCDoesNotPumpSilence(t *testing.T) {
	var a agc
	sil := make([]int16, frameSize) // pure silence
	for i := 0; i < 200; i++ {
		a.process(sil)
	}
	if a.gain != 1 {
		t.Fatalf("gain must stay 1 on silence, got %.2f", a.gain)
	}
}

func TestAGCTamesLoudWithoutClipRunaway(t *testing.T) {
	var a agc
	loud := sine(30000, 300)
	var last []int16
	for i := 0; i < 400; i++ {
		last = append([]int16(nil), loud...)
		a.process(last)
	}
	if a.gain > 1 {
		t.Fatalf("loud input should pull gain below 1, got %.2f", a.gain)
	}
	if r := rmsOf(last); r > agcTargetRMS*1.4 {
		t.Fatalf("loud speech not tamed toward target: rms %.0f", r)
	}
}
