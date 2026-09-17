package main

import (
	"math"
	"math/rand"
	"testing"

	"github.com/demoj1/yap/internal/aec"
)

// feed runs frames through the tracker: speech-like bursts on the speakers
// and a mic that hears them lag frames later (attenuated) plus noise, or
// only noise when lag < 0. Returns the tracker after the last estimate.
func feed(lag int, frames int) *echoTracker {
	rng := rand.New(rand.NewSource(7))
	e := &echoTracker{}
	spk := make([][]int16, frames)
	for f := range spk {
		spk[f] = make([]int16, frameSize/2)
		if f/12%3 != 0 { // 240 ms bursts, 120 ms pauses
			for i := range spk[f] {
				spk[f][i] = int16(rng.NormFloat64() * 8000)
			}
		}
	}
	mic := make([]int16, frameSize/2)
	for f := range spk {
		for i := range mic {
			mic[i] = int16(rng.NormFloat64() * 300) // room noise
			if lag >= 0 && f-lag >= 0 {
				mic[i] += spk[f-lag][i] / 3
			}
		}
		if e.push(spk[f], mic) {
			e.estimate()
		}
	}
	return e
}

func TestEchoTrackerFindsLag(t *testing.T) {
	for _, lag := range []int{3, 12, 30} {
		e := feed(lag, 2*echoHist)
		if !e.echo || e.lag != lag {
			t.Errorf("lag %d: got echo=%v lag=%d", lag, e.echo, e.lag)
		}
	}
}

func TestEchoTrackerHearsNoEchoOnHeadphones(t *testing.T) {
	if e := feed(-1, 2*echoHist); e.echo {
		t.Errorf("no echo path, yet echo=true at lag %d", e.lag)
	}
}

// The whole path: a 200 ms echo — as long as the canceller's tail —
// is still cancelled, because the tracker feeds the reference from 200 ms
// back and only the room's reverb has to fit in the tail.
func TestTrackerAlignsCancellerBeyondTail(t *testing.T) {
	const lag, frames = 20, 4 * echoHist
	rng := rand.New(rand.NewSource(3))
	e := &echoTracker{}
	c := aec.New(frameSize/2, sampleRate*aecTailMS/1000, sampleRate)
	defer c.Close()
	spk := make([][]int16, frames)
	for f := range spk {
		spk[f] = make([]int16, frameSize/2)
		for i := range spk[f] {
			spk[f][i] = int16(rng.NormFloat64() * 6000)
		}
	}
	var in, out float64
	mic := make([]int16, frameSize/2)
	for f := range spk {
		for i := range mic {
			mic[i] = 0
			if f >= lag {
				mic[i] = spk[f-lag][i] / 2
			}
		}
		if e.push(spk[f], mic) && e.estimate() {
			c.Reset()
		}
		measure := f > frames-echoHist // the last 3 s, long after alignment and adaptation
		if measure {
			in += sumSq(mic)
		}
		if e.echo {
			c.Process(mic, e.reference())
		}
		if measure {
			out += sumSq(mic)
		}
	}
	erle := 10 * math.Log10(in/out)
	t.Logf("lag %d found (echo=%v), echo reduction %.1f dB", e.lag, e.echo, erle)
	if e.lag != lag || erle < 12 {
		t.Fatalf("lag %d, %.1f dB: expected lag %d and >12 dB", e.lag, erle, lag)
	}
}
