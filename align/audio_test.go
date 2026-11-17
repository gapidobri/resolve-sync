package align

import (
	"math"
	"math/rand"
	"testing"
)

func TestCorrelate(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	src := make([]float32, 40000)
	for i := range src {
		src[i] = float32(rng.NormFloat64())
	}
	// b hears the same sound 1234 samples into a, quieter and with its own noise.
	const lag = 1234
	a := src
	b := make([]float32, 8000)
	for i := range b {
		b[i] = 0.3*src[i+lag] + 0.2*float32(rng.NormFloat64())
	}
	for _, whiten := range []bool{false, true} {
		p, ok := Correlate(a, b, whiten, 50)
		if !ok || p.Offset != lag || p.Score < 3*p.RunnerUp {
			t.Errorf("whiten=%v: %+v", whiten, p)
		}
	}
	// Unrelated sound gives no standout peak.
	c := make([]float32, 8000)
	for i := range c {
		c[i] = float32(rng.NormFloat64())
	}
	if p, _ := Correlate(a, c, true, 50); p.Score > 1.5*p.RunnerUp {
		t.Errorf("false match: %+v", p)
	}
}

func TestEnvelope(t *testing.T) {
	// A tone that gets louder every half second.
	s := make([]float32, 8000*4)
	for i := range s {
		burst := 0.1
		if (i/4000)%2 == 1 {
			burst = 0.8
		}
		s[i] = float32(burst * math.Sin(float64(i)*0.3))
	}
	env := Envelope(s, 160, 100)
	if len(env) != 200 || env[30] <= env[10] {
		t.Errorf("len %d, quiet %.2f loud %.2f", len(env), env[10], env[30])
	}
}

func TestWaveform(t *testing.T) {
	// Two seconds at 4 values a second: silence, then full scale.
	env := []float32{-30, -30, -30, -30, 0, -1, -2, 0}
	w := waveform(env, 4)
	if len(w) != 2 || w[0] != 0 || w[1] != 255 {
		t.Errorf("waveform = %v", w)
	}
}
