package main

import "testing"

func TestPeak(t *testing.T) {
	var p peak
	p.observe([]int16{0, -3277, 100})
	if db := p.take(); db < -20.1 || db > -19.9 {
		t.Fatalf("want -20 dBFS got %.2f", db)
	}
	var m meter
	m.feed(-30, 1)
	if m.level < 0.39 || m.level > 0.41 {
		t.Fatalf("level want 0.4 got %.2f", m.level)
	}
}
