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
	for s := 0; s < prebuf; s++ {
		push(j, s)
	}
	for s := 0; s < prebuf; s++ {
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
		t.Fatal("must not start before prebuf")
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

func TestSlowSenderGetsPLC(t *testing.T) {
	j := newJitter()
	fill(j)
	if s, _ := pull(t, j); s != -2 {
		t.Fatal("first empty pull must be PLC")
	}
	push(j, prebuf+1)
	if s, _ := pull(t, j); s != prebuf+1 {
		t.Fatalf("stream continues after PLC, want %d got %d", prebuf+1, s)
	}
}

func TestStarveRebuffers(t *testing.T) {
	j := newJitter()
	fill(j)
	for i := 0; i < prebuf; i++ {
		if s, _ := pull(t, j); s != -2 {
			t.Fatal("expected PLC")
		}
	}
	if s, _ := pull(t, j); s != -1 {
		t.Fatal("expected rebuffer after prolonged starvation")
	}
	for s := 10; s < 10+prebuf-1; s++ {
		push(j, s)
	}
	if s, _ := pull(t, j); s != -1 {
		t.Fatal("must wait for prebuf")
	}
	push(j, 10+prebuf-1)
	if s, _ := pull(t, j); s != 10 {
		t.Fatalf("want 10 got %d", s)
	}
}

func TestSkipAheadWhenDeep(t *testing.T) {
	j := newJitter()
	for s := 0; s <= maxDepth; s++ {
		push(j, s)
	}
	if s, _ := pull(t, j); s != maxDepth-prebuf {
		t.Fatalf("want %d got %d", maxDepth-prebuf, s)
	}
}
