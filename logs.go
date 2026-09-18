package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// `yap logs <link>` joins the room without a microphone or speakers, asks
// everyone it reaches for their log, and leaves with the files. Every
// participant hears about it in the chat. What comes back is the tail of
// yap.log and stats.jsonl, so a call gone wrong can be read from all sides.

const (
	logsTail  = 256 << 10 // bytes of yap.log per person
	statsTail = 64 << 10
	logsWait  = 90 * time.Second
)

// runCollector is the collector's life: like a relay, no audio, but it
// asks for logs instead of forwarding, and stops once everyone answered.
func (n *node) runCollector() {
	n.collector = true
	go n.recvLoop()
	go n.reaper()
	go n.pingLoop()
	go n.rendezvous()
	deadline := time.After(logsWait)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			n.collectorDone("time is up")
			return
		case <-tick.C:
			waiting := 0
			for _, p := range n.people() {
				if !p.logsGot.Load() {
					waiting++
				}
			}
			if len(n.people()) > 0 && waiting == 0 {
				time.Sleep(2 * time.Second) // let the last "done" go out
				n.collectorDone("everyone answered")
				return
			}
		}
	}
}

func (n *node) collectorDone(why string) {
	fmt.Printf("\n%s — logs in %s:\n", why, n.files.dir)
	for _, p := range n.people() {
		state := "no answer"
		if p.logsGot.Load() {
			state = "ok"
		}
		fmt.Printf("  %-16s %s\n", p.name, state)
	}
}

// askLogs is sent to a person as soon as they are reachable.
func (n *node) askLogs(p *peer) {
	if n.collector && !p.relay {
		n.sendTo(p, []byte{typLogs})
	}
}

// giveLogs answers a request: the tails of our log and stats go back as a
// file, and the room is told.
func (n *node) giveLogs(p *peer) {
	if n.relay || n.collector {
		return
	}
	n.system("%s collected your log", p.name)
	body := fmt.Sprintf("# %s · %s · %s\n\n## yap.log (last %d KB)\n\n", n.name, version, time.Now().Format(time.RFC3339), logsTail>>10)
	body += tailOf(filepath.Join(configDir(), "yap.log"), logsTail)
	body += fmt.Sprintf("\n\n## stats.jsonl (last %d KB)\n\n", statsTail>>10)
	body += tailOf(filepath.Join(configDir(), "stats.jsonl"), statsTail)
	go n.pushFile(p, uint32(time.Now().Unix()), n.name+".log", []byte(body))
}

func tailOf(path string, max int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(" + err.Error() + ")"
	}
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}
