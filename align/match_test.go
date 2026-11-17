package align

import (
	"math"
	"math/rand"
	"testing"
)

// scene returns a picture that drifts smoothly with time on top of a fixed
// background, like a stage with moving lights.
type scene struct {
	bg     Frame
	comps  []Frame
	speeds []float64
}

func newScene(rng *rand.Rand, size int) *scene {
	sc := &scene{bg: make(Frame, size)}
	for i := range sc.bg {
		sc.bg[i] = 80 + 60*rng.Float32()
	}
	for c := 0; c < 12; c++ {
		f := make(Frame, size)
		for i := range f {
			f[i] = rng.Float32() - 0.5
		}
		sc.comps = append(sc.comps, f)
		sc.speeds = append(sc.speeds, 0.2+3*rng.Float64())
	}
	return sc
}

// at renders the scene at time t with a given gain and lift (a different
// "look") and a little noise.
func (sc *scene) at(t float64, gain, lift float32, rng *rand.Rand) Frame {
	f := make(Frame, len(sc.bg))
	copy(f, sc.bg)
	for c, comp := range sc.comps {
		a := float32(20 * math.Sin(sc.speeds[c]*t+float64(c)))
		for i := range f {
			f[i] += a * comp[i]
		}
	}
	for i := range f {
		f[i] = gain*f[i] + lift + float32(rng.NormFloat64())
	}
	return f
}

func TestCoarse(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	sc := newScene(rng, 32*18)
	// ISO track: one frame a second for 15 minutes, starting at ISO time 0.
	var track []Frame
	for i := 0; i < 900; i++ {
		track = append(track, sc.at(float64(i), 1, 0, rng))
	}
	// The raw clip started 281.3 s after the ISO, and looks flatter.
	const offset = 281.3
	var samples []Frame
	var times []float64
	for k := 0; k < 24; k++ {
		rt := 20 + 10*float64(k)
		times = append(times, rt)
		samples = append(samples, sc.at(rt+offset, 0.6, 30, rng))
	}
	p, ok := Coarse(samples, times, track, 0, 1)
	if !ok || math.Abs(p.Offset-offset) > 0.5 {
		t.Errorf("peak = %+v, ok = %v", p, ok)
	}
}

func TestFine(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	sc := newScene(rng, 64*36)
	const offset = 281.3137 // true ISO time minus raw time
	const rawStart, isoStart = 100.0, 380.0
	var raw, iso []Frame
	for i := 0; i < 100; i++ { // 4 s at 25 fps
		raw = append(raw, sc.at(rawStart+float64(i)/25+offset, 0.6, 30, rng))
	}
	for j := 0; j < 420; j++ { // 7 s at 60 fps
		iso = append(iso, sc.at(isoStart+float64(j)/60, 1, 0, rng))
	}
	p, ok := Fine(raw, rawStart, 25, iso, isoStart, 60, 281, 1.2)
	if !ok || math.Abs(p.Offset-offset) > 0.004 {
		t.Errorf("peak = %+v, want offset %.4f", p, offset)
	}
}
