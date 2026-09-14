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
	if s, _ := pull(t, j); s != -1 {
		t.Fatal("late packet must be dropped")
	}
}

func TestStarveRebuffers(t *testing.T) {
	j := newJitter()
	push(j, 0, 1)
	pull(t, j)
	pull(t, j)
	if s, _ := pull(t, j); s != -1 {
		t.Fatal("expected starvation")
	}
	push(j, 2)
	if s, _ := pull(t, j); s != -1 {
		t.Fatal("must rebuffer after starvation")
	}
	push(j, 3)
	if s, _ := pull(t, j); s != 2 {
		t.Fatalf("want 2 got %d", s)
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
