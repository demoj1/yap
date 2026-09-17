package main

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/gen2brain/malgo"
)

// Everything a user can flip or turn lives here, so the terminal and the
// browser drive the same switches and knobs.

// toggle is one on/off switch: its key, label, state, what flipping it does
// (returning the notice), and a live reading shown while it is on.
type toggle struct {
	key, label string
	on         func() bool
	flip       func() string
	live       func() string
	offRed     bool // off is the alarming state (an open mic that is muted)
}

// toggles is the single list behind the chip row, the letter keys and the
// web page's buttons.
func (n *node) toggles() []toggle {
	ctl, set := n.ctl, n.set
	onOff := func(b bool) string { return map[bool]string{true: "on", false: "off"}[b] }
	// saved flips a setting that is remembered across runs; after runs once it is applied.
	saved := func(b *atomic.Bool, remembered *bool, name string, after func()) func() string {
		return func() string {
			b.Store(!b.Load())
			*remembered = b.Load()
			set.save()
			if after != nil {
				after()
			}
			return name + " " + onOff(b.Load())
		}
	}
	return []toggle{
		{"m", "mic", func() bool { return !ctl.muted.Load() }, func() string {
			ctl.muted.Store(!ctl.muted.Load())
			n.sendState()
			return "mic " + map[bool]string{true: "MUTED", false: "on"}[ctl.muted.Load()]
		}, nil, true},
		{"d", "denoise", ctl.denoise.Load, saved(&ctl.denoise, &set.Denoise, "denoise", nil), nil, false},
		{"g", "gate", ctl.gate.Load, saved(&ctl.gate, &set.Gate, "noise gate", nil),
			func() string { return map[bool]string{true: "open", false: "shut"}[n.gateOpen.Load()] }, false},
		{"e", "echo", ctl.aec.Load, saved(&ctl.aec, &set.Echo, "echo cancel", func() { n.audio.aecOn.Store(ctl.aec.Load()) }),
			func() string {
				if !n.audio.echoing.Load() {
					return "no echo"
				}
				return fmt.Sprintf("−%.0f dB @ %d ms", max(0, math.Float64frombits(n.audio.aecDB.Load())), n.audio.echoLag.Load())
			}, false},
		{"a", "gain", ctl.agc.Load, saved(&ctl.agc, &set.AGC, "auto-gain", nil),
			func() string { return fmt.Sprintf("×%.1f", math.Float64frombits(n.agcGain.Load())) }, false},
		{"l", "lock", n.locked.Load, n.toggleLock, nil, false},
		{"p", "ptt", ctl.ptt.Load, saved(&ctl.ptt, &set.PTT, "push-to-talk (hold space)", n.sendState), nil, false},
		{"s", "sounds", ctl.sounds.Load, saved(&ctl.sounds, &set.Sounds, "sounds", nil), nil, false},
		{"w", "web", n.web.running, func() string {
			set.Web = !n.web.running()
			set.save()
			if set.Web {
				return n.web.start()
			}
			n.web.stop()
			return "web off"
		}, n.web.url, false},
	}
}

// press flips the toggle bound to key; "" if there is none.
func (n *node) press(key string) string {
	for _, t := range n.toggles() {
		if t.key == key {
			return t.flip()
		}
	}
	return ""
}

// knob is one tunable number on the tuning tile.
type knob struct {
	name, unit   string
	get          func() int
	set          func(int)
	step, lo, hi int
}

// knobs lists the echo canceller settings; every change is saved and
// applied live.
func (n *node) knobs() []knob {
	s := n.set
	apply := func() {
		s.save()
		n.audio.setAEC(s.AECSuppress, s.AECSuppressActive)
	}
	return []knob{
		{"echo suppress", "dB", func() int { return s.AECSuppress }, func(v int) { s.AECSuppress = v; apply() }, 5, -80, -10},
		{"echo suppress while they talk", "dB", func() int { return s.AECSuppressActive }, func(v int) { s.AECSuppressActive = v; apply() }, 5, -50, -5},
	}
}

// turnKnob nudges knob i by dir steps within its range and returns the notice.
func (n *node) turnKnob(i, dir int) string {
	k := n.knobs()[i]
	v := max(k.lo, min(k.hi, k.get()+dir*k.step))
	k.set(v)
	return fmt.Sprintf("%s %d %s", k.name, v, k.unit)
}

// nudgeVolume moves a person's volume by dir steps and returns the notice.
func (n *node) nudgeVolume(p *peer, dir int) string {
	v := max(0, min(200, int(p.volume.Load())+dir*volStep))
	n.setVolume(p, v)
	return fmt.Sprintf("%s volume %d%%", p.name, v)
}

// nudgeBitrate moves our bitrate one notch, remembers it and returns the notice.
func (n *node) nudgeBitrate(dir int) string {
	n.ctl.stepBitrate(dir)
	n.set.Bitrate = int(n.ctl.bitrate.Load())
	n.set.save()
	return fmt.Sprintf("your bitrate %d kbps", n.set.Bitrate)
}

const volStep = 3 // percent per volume nudge

// levelLoop samples every meter 20 times a second into plain values both
// the terminal and the browser read, so neither steals the other's peaks.
func (n *node) levelLoop() {
	for range time.Tick(50 * time.Millisecond) {
		n.micDB.Store(math.Float64bits(n.audio.micPeak.take()))
		for _, p := range n.peerList() {
			p.levelDB.Store(math.Float64bits(p.level.take()))
		}
	}
}

// deviceLists are the input and output device names as last refreshed.
func (n *node) deviceLists() (inputs, outputs []string) {
	if d := n.devices.Load(); d != nil {
		return d[0], d[1]
	}
	return nil, nil
}

// devicesLoop refreshes the device lists every few seconds; enumerating
// spins up a miniaudio context, too costly per frame.
func (n *node) devicesLoop() {
	for {
		in, _ := n.deviceNames(malgo.Capture)
		out, _ := n.deviceNames(malgo.Playback)
		n.devices.Store(&[2][]string{in, out})
		time.Sleep(5 * time.Second)
	}
}
