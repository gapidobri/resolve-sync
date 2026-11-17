package syncer

import (
	"math"
	"testing"
)

func TestFitLine(t *testing.T) {
	// Offset 100 s at the start, losing 10 ms per 1000 s.
	m := []match{{100, 99.999}, {1000, 99.99}, {3000, 99.97}, {5000, 99.95}}
	slope, intercept := fitLine(m)
	if math.Abs(slope+1e-5) > 1e-9 || math.Abs(intercept-100) > 1e-6 {
		t.Errorf("slope %g intercept %g", slope, intercept)
	}
	if s, i := fitLine([]match{{5, 7}, {5, 9}}); s != 0 || i != 8 {
		t.Errorf("degenerate: %g %g", s, i)
	}
}

func TestResembles(t *testing.T) {
	for _, c := range []struct {
		folder, input string
		want          bool
	}{
		{"Total", "4 - total", true}, {"Dinamicna", "1 - din pero", true},
		{"Left", "2 - levo", false}, {"FOH", "3 - center", false},
	} {
		if got := resembles(c.folder, c.input); got != c.want {
			t.Errorf("resembles(%q, %q) = %v", c.folder, c.input, got)
		}
	}
}

func TestSoundLag(t *testing.T) {
	// Deterministic noise; b is a's contour from 3 s in.
	a := make([]float32, 4000)
	x := uint32(1)
	for i := range a {
		x = x*1664525 + 1013904223
		a[i] = float32(x>>8)/float32(1<<24) - 0.5
	}
	b := append([]float32(nil), a[600:1800]...)
	lag, ok := soundLag(a, b)
	if !ok || math.Abs(lag-600*hopSec) > 1e-9 {
		t.Errorf("lag %g ok %v", lag, ok)
	}
	c := make([]float32, 1200)
	for i := range c {
		x = x*1664525 + 1013904223
		c[i] = float32(x>>8)/float32(1<<24) - 0.5
	}
	if _, ok := soundLag(a, c); ok {
		t.Error("unrelated sound must not match")
	}
}
