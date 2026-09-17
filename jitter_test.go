package main

import "testing"

func pull(t *testing.T, j *jitter) (seq int, lost bool) {
	t.Helper()
	p, lost, ok := j.pull()
	if !ok {
		return -1, false
	}
	if lost {
		return -2, true
	}
	return int(p[0]), false
}

func fill(j *jitter) {
	for s := 0; s < minPrebuf; s++ {
		push(j, s)
	}
	for s := 0; s < minPrebuf; s++ {
		j.pull()
	}
}

func push(j *jitter, seqs ...int) {
	for _, s := range seqs {
		j.push(uint64(s), []byte{byte(s)})
	}
}

func TestPrebufAndOrder(t *testing.T) {
	j := newJitter()
	push(j, 0)
	if s, _ := pull(t, j); s != -1 {
		t.Fatal("must not start before minPrebuf")
	}
	for i := minPrebuf - 1; i >= 1; i-- { // out of order on purpose
		push(j, i)
	}
	for want := 0; want < minPrebuf; want++ {
		if s, _ := pull(t, j); s != want {
			t.Fatalf("want %d got %d", want, s)
		}
	}
}

func TestGapIsLoss(t *testing.T) {
	j := newJitter()
	for i := 0; i <= minPrebuf; i++ {
		if i != 2 {
			push(j, i) // seq 2 never arrives in time
		}
	}
	pull(t, j)
	pull(t, j)
	if s, lost := pull(t, j); !lost || s != -2 {
		t.Fatal("expected PLC for seq 2")
	}
	if s, _ := pull(t, j); s != 3 {
		t.Fatalf("want 3 got %d", s)
	}
	for j.depth() > 0 { // drain so the buffer is starved when the late one lands
		pull(t, j)
	}
	push(j, 2)
	if s, _ := pull(t, j); s != -2 {
		t.Fatal("late packet must be dropped, buffer stays starved")
	}
	if j.late.Load() != 1 {
		t.Fatal("late must be counted")
	}
}

func TestSlowSenderStalls(t *testing.T) {
	j := newJitter()
	fill(j)
	if s, _ := pull(t, j); s != -2 {
		t.Fatal("first empty pull must be PLC")
	}
	push(j, minPrebuf)
	if s, _ := pull(t, j); s != minPrebuf {
		t.Fatalf("late frame must still be played, want %d got %d", minPrebuf, s)
	}
	if j.stall.Load() != 1 || j.lost.Load() != 0 {
		t.Fatalf("stall 1 lost 0 expected, got stall %d lost %d", j.stall.Load(), j.lost.Load())
	}
}

func TestStarveRebuffers(t *testing.T) {
	j := newJitter()
	fill(j)
	for i := 0; i < minPrebuf; i++ {
		if s, _ := pull(t, j); s != -2 {
			t.Fatal("expected PLC")
		}
	}
	if s, _ := pull(t, j); s != -1 {
		t.Fatal("expected rebuffer after prolonged starvation")
	}
	if j.target() != minPrebuf+1 {
		t.Fatalf("prebuf must grow after a rebuffer, got %d", j.target())
	}
	want := j.target()
	for s := 10; s < 10+want-1; s++ {
		push(j, s)
	}
	if s, _ := pull(t, j); s != -1 {
		t.Fatal("must wait for the grown prebuf")
	}
	push(j, 10+want-1)
	if s, _ := pull(t, j); s != 10 {
		t.Fatalf("want 10 got %d", s)
	}
}

func TestPrebufRelaxes(t *testing.T) {
	j := newJitter()
	j.prebuf = maxPrebuf // pretend a burst grew it
	seq := 0
	for i := 0; i < maxPrebuf; i++ {
		push(j, seq)
		seq++
	}
	for i := 0; i < relaxRuns*2; i++ {
		push(j, seq)
		seq++
		pull(t, j)
	}
	if j.target() >= maxPrebuf {
		t.Fatalf("prebuf must relax after clean runs, still %d", j.target())
	}
}

func TestSkipAheadWhenDeep(t *testing.T) {
	j := newJitter()
	for s := 0; s <= maxDepth; s++ {
		push(j, s)
	}
	if s, _ := pull(t, j); s != maxDepth-minPrebuf {
		t.Fatalf("want %d got %d", maxDepth-minPrebuf, s)
	}
}

// A gap with the next packet already there hands that packet over with
// lost=true, so the decoder can rebuild the missing frame from its FEC data.
func TestJitterHandsOverFECPacket(t *testing.T) {
	j := newJitter()
	for seq := uint64(0); seq < uint64(minPrebuf)+2; seq++ {
		if seq != 2 {
			j.push(seq, []byte{byte(seq)})
		}
	}
	j.pull() // 0
	j.pull() // 1
	pkt, lost, ok := j.pull() // 2 is missing, 3 is there
	if !ok || !lost || len(pkt) != 1 || pkt[0] != 3 {
		t.Fatalf("gap: pkt=%v lost=%v ok=%v, want packet 3 with lost", pkt, lost, ok)
	}
	if pkt, lost, _ := j.pull(); lost || pkt[0] != 3 {
		t.Fatalf("after the gap packet 3 must play normally, got %v lost=%v", pkt, lost)
	}
}
