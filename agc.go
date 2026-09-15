package main

import "math"

// agc is automatic gain control for the outgoing voice: it measures the
// speech level and moves a smooth gain toward a target so quiet talkers come
// up and loud ones come down, without pumping on background noise (it only
// adapts on frames above the floor) and without clipping (gain drops fast,
// rises slow, and the output is hard-limited).
type agc struct {
	gain float64
}

const (
	agcTargetRMS = 5000 // desired speech RMS in int16 units (~ -16 dBFS)
	agcFloorRMS  = 300  // below this a frame is noise/silence: hold gain, don't adapt
	agcMaxGain   = 10.0 // ceiling so silence/hiss is never blown up
	agcMinGain   = 0.3  // floor so a shout is tamed, not muted
	agcAttack    = 0.02 // per frame: raise gain slowly (no pumping)
	agcRelease   = 0.25 // per frame: lower gain fast (stay ahead of clipping)
)

func (a *agc) process(pcm []int16) {
	if a.gain == 0 {
		a.gain = 1
	}
	var sum float64
	for _, x := range pcm {
		sum += float64(x) * float64(x)
	}
	rms := math.Sqrt(sum / float64(len(pcm)))
	if rms >= agcFloorRMS {
		want := max(agcMinGain, min(agcMaxGain, agcTargetRMS/rms))
		rate := agcAttack
		if want < a.gain {
			rate = agcRelease
		}
		a.gain += (want - a.gain) * rate
	}
	if a.gain == 1 {
		return
	}
	for i, x := range pcm {
		pcm[i] = int16(max(-32768, min(32767, int32(float64(x)*a.gain))))
	}
}
