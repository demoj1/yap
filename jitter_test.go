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
	push(j, 2, 1)
	for want := 0; want < 3; want++ {
		if s, _ := pull(t, j); s != want {
			t.Fatalf("want %d got %d", want, s)
		}
	}
}

func TestGapIsLoss(t *testing.T) {
	j := newJitter()
	push(j, 0, 1, 3)
	pull(t, j)
	pull(t, j)
	if s, lost := pull(t, j); !lost || s != -2 {
		t.Fatal("expected PLC for seq 2")
	}
	if s, _ := pull(t, j); s != 3 {
		t.Fatalf("want 3 got %d", s)
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
