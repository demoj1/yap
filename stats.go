package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Connection statistics: one JSON line per event in <config>/yap/stats.jsonl,
// so a bad call can be analysed afterwards without grepping the log. Events
// are start, hello, connect (with how it went and how long it took), gone,
// and a sample per peer every statsSample.
var statsFile *os.File

const statsSample = 30 * time.Second

func openStats() {
	path := filepath.Join(configDir(), "stats.jsonl")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		flags |= os.O_TRUNC
	}
	statsFile, _ = os.OpenFile(path, flags, 0o600) // no stats is not worth failing a call over
}

// stat records one event with its fields.
func stat(event string, fields map[string]any) {
	if statsFile == nil {
		return
	}
	fields["t"] = time.Now().Format(time.RFC3339)
	fields["e"] = event
	raw, _ := json.Marshal(fields)
	statsFile.Write(append(raw, '\n'))
}

// peerStat is the periodic sample of one peer.
func peerStat(p *peer) map[string]any {
	path := "none"
	switch {
	case p.direct():
		path = "direct"
	case p.via.Load() != nil:
		path = "via " + p.via.Load().name
	}
	return map[string]any{"peer": p.name, "ver": p.ver, "path": path,
		"rtt_ms": p.rttUS.Load() / 1000, "jitter_ms": float64(p.jitUS.Load()) / 1000,
		"lost": p.jb.lost.Load(), "stall": p.jb.stall.Load(), "skip": p.jb.skip.Load(), "rebuf": p.jb.rebuf.Load(),
		"rx": p.rx.Load(), "tx": p.tx.Load()}
}

// printStats sums stats.jsonl up per peer: how often and how fast they
// connected, which way, and how the audio from them held up.
func printStats() {
	f, err := os.Open(filepath.Join(configDir(), "stats.jsonl"))
	if err != nil {
		fmt.Println("no stats yet:", err)
		return
	}
	defer f.Close()
	type tally struct {
		hellos, direct, via, failed, samples int
		connectS                             []float64
		rtt, jitter, drops                   float64
		drop0                                float64
	}
	peers := map[string]*tally{}
	get := func(name string) *tally {
		if peers[name] == nil {
			peers[name] = &tally{}
		}
		return peers[name]
	}
	sessions := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		name, _ := ev["peer"].(string)
		switch ev["e"] {
		case "start":
			sessions++
		case "hello":
			get(name).hellos++
		case "connect":
			t := get(name)
			switch ev["how"] {
			case "direct":
				t.direct++
			case "via":
				t.via++
			default:
				t.failed++
			}
			if s, ok := ev["after_s"].(float64); ok && ev["how"] != "none" {
				t.connectS = append(t.connectS, s)
			}
		case "peer":
			t := get(name)
			t.samples++
			t.rtt += num(ev["rtt_ms"])
			t.jitter += num(ev["jitter_ms"])
			t.drops = num(ev["lost"]) + num(ev["stall"]) + num(ev["skip"]) + num(ev["rebuf"]) // cumulative: the last sample counts
		}
	}
	names := make([]string, 0, len(peers))
	for n := range peers {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("%d sessions\n\n%-14s %6s %7s %5s %6s %9s %8s %9s %7s\n", sessions, "peer", "hellos", "direct", "via", "failed", "connect s", "rtt ms", "jitter ms", "drops")
	for _, n := range names {
		t := peers[n]
		med := "-"
		if len(t.connectS) > 0 {
			sort.Float64s(t.connectS)
			med = fmt.Sprintf("%.1f", t.connectS[len(t.connectS)/2])
		}
		rtt, jit := "-", "-"
		if t.samples > 0 {
			rtt, jit = fmt.Sprintf("%.0f", t.rtt/float64(t.samples)), fmt.Sprintf("%.1f", t.jitter/float64(t.samples))
		}
		fmt.Printf("%-14s %6d %7d %5d %6d %9s %8s %9s %7.0f\n", n, t.hellos, t.direct, t.via, t.failed, med, rtt, jit, t.drops)
	}
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}
