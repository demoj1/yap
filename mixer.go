package main

// limiter keeps a summed mix inside int16 without hard clipping: when a
// frame would overshoot, gain drops instantly to fit it and then eases back
// toward unity over ~0.4 s, so several people talking at once get quieter
// together instead of crackling.
type limiter struct {
	gain float64 // current gain, 1 = untouched
}

const limiterRelease = 0.05 // per 20 ms frame: 1 - 0.05^20 ≈ back to unity in ~0.4 s

// apply writes mix into out through the limiter.
func (l *limiter) apply(mix []int32, out []int16) {
	if l.gain == 0 {
		l.gain = 1
	}
	var peak int32
	for _, x := range mix {
		if x < 0 {
			x = -x
		}
		if x > peak {
			peak = x
		}
	}
	want := 1.0
	if peak > 32767 {
		want = 32767 / float64(peak)
	}
	if want < l.gain {
		l.gain = want
	} else {
		l.gain += (want - l.gain) * limiterRelease
	}
	for i, x := range mix {
		out[i] = int16(max(-32768, min(32767, int32(float64(x)*l.gain))))
	}
}

// mixInto adds pcm scaled by volume (percent) into mix.
func mixInto(mix []int32, pcm []int16, volume int32) {
	for i, x := range pcm {
		mix[i] += int32(x) * volume / 100
	}
}
