package main

import "testing"

func TestGateOpensOnSpeechHoldsThenCloses(t *testing.T) {
	var g gate
	quiet := make([]int16, frameSize) // silence
	loud := sine(15000, 300)

	if g.pass(quiet) {
		t.Fatal("gate must start closed on silence")
	}
	if !g.pass(loud) {
		t.Fatal("gate must open on speech")
	}
	// A short quiet gap stays open (hold), so word tails are not clipped.
	for i := 0; i < gateHold-1; i++ {
		if !g.pass(quiet) {
			t.Fatalf("gate closed too early at hold frame %d", i)
		}
	}
	// After the hold runs out, it closes.
	closed := false
	for i := 0; i < gateHold+2; i++ {
		if !g.pass(quiet) {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("gate never closed after the hold")
	}
}
