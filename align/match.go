package align

import (
	"math"
	"sort"
)

// center subtracts the average picture from every frame, so that what is
// left is how each moment differs from the typical view. A locked-off camera
// looks nearly identical all evening; the differences are what tell one
// moment from another. It also removes most of the look difference between
// the camera's own recording and its HDMI feed.
func center(frames []Frame) []Frame {
	if len(frames) == 0 {
		return nil
	}
	mean := make([]float64, len(frames[0]))
	for _, f := range frames {
		for i, v := range f {
			mean[i] += float64(v)
		}
	}
	for i := range mean {
		mean[i] /= float64(len(frames))
	}
	out := make([]Frame, len(frames))
	for k, f := range frames {
		g := make(Frame, len(f))
		var norm float64
		for i, v := range f {
			d := float64(v) - mean[i]
			g[i] = float32(d)
			norm += d * d
		}
		// Unit length, so a dot product between two frames is their cosine.
		if norm = math.Sqrt(norm); norm > 0 {
			for i := range g {
				g[i] = float32(float64(g[i]) / norm)
			}
		}
		out[k] = g
	}
	return out
}

func dot(a, b Frame) float64 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return float64(s)
}

// Peak describes how well the best candidate stands out.
type Peak struct {
	Offset float64 // seconds to add to a raw clip time to get the ISO file time
	Score  float64 // similarity at the best offset, -1..1
	// RunnerUp is the best score among candidates that are clearly a
	// different answer. A real match beats it by a wide gap; footage of the
	// wrong camera or the wrong moment does not.
	RunnerUp float64
	// Margin is how far the best score is above the typical score, in units
	// of the spread of all scores.
	Margin float64
}

// Lead is the gap between the best candidate and the best different answer.
func (p Peak) Lead() float64 { return p.Score - p.RunnerUp }

// pick returns the best candidate. Candidates within exclude seconds of it
// count as the same answer when looking for the runner-up.
func pick(offsets, scores []float64, exclude float64) Peak {
	best := 0
	for i, v := range scores {
		if v > scores[best] {
			best = i
		}
	}
	runner := math.Inf(-1)
	for i, v := range scores {
		if math.Abs(offsets[i]-offsets[best]) > exclude && v > runner {
			runner = v
		}
	}
	if math.IsInf(runner, -1) {
		runner = scores[best]
	}
	median, spread := stats(scores)
	return Peak{Offset: offsets[best], Score: scores[best], RunnerUp: runner, Margin: (scores[best] - median) / spread}
}

// stats returns the median and a robust spread of v.
func stats(v []float64) (median, spread float64) {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	median = s[len(s)/2]
	dev := make([]float64, len(s))
	for i, x := range s {
		dev[i] = math.Abs(x - median)
	}
	sort.Float64s(dev)
	spread = 1.4826 * dev[len(dev)/2]
	if spread < 1e-9 {
		spread = 1e-9
	}
	return
}

// Coarse finds the offset between a raw clip and an ISO track to within
// about a second.
//
// samples are single frames of the raw clip at rawTimes (seconds into the
// clip); track is the ISO sampled every step seconds from trackStart. Every
// whole-step offset that keeps at least half the samples inside the track is
// scored.
func Coarse(samples []Frame, rawTimes []float64, track []Frame, trackStart, step float64) (Peak, bool) {
	if len(samples) < 4 || len(track) < 4 {
		return Peak{}, false
	}
	s, t := center(samples), center(track)
	trackEnd := trackStart + float64(len(track)-1)*step
	lo := math.Ceil((trackStart - rawTimes[len(rawTimes)-1]) / step)
	hi := math.Floor((trackEnd - rawTimes[0]) / step)
	var offsets, scores []float64
	for k := lo; k <= hi; k++ {
		o := k * step
		var sum float64
		var n int
		for i, rt := range rawTimes {
			j := int(math.Round((rt + o - trackStart) / step))
			if j < 0 || j >= len(t) {
				continue
			}
			sum += dot(s[i], t[j])
			n++
		}
		if n*2 < len(samples) {
			continue
		}
		offsets = append(offsets, o)
		scores = append(scores, sum/float64(n))
	}
	if len(scores) < 8 {
		return Peak{}, false
	}
	return pick(offsets, scores, 5*step), true
}

// Fine refines an offset to a fraction of a frame.
//
// raw holds consecutive frames of the raw clip starting at rawStart seconds,
// iso consecutive frames of the ISO file starting at isoStart seconds. Lags
// within maxLag seconds of guess are scored in steps of a tenth of the
// shorter frame duration.
func Fine(raw []Frame, rawStart, rawRate float64, iso []Frame, isoStart, isoRate float64, guess, maxLag float64) (Peak, bool) {
	if len(raw) < 8 || len(iso) < 8 {
		return Peak{}, false
	}
	r, s := center(raw), center(iso)
	// Similarity of every raw frame to every ISO frame.
	sim := make([][]float64, len(r))
	for i := range r {
		sim[i] = make([]float64, len(s))
		for j := range s {
			sim[i][j] = dot(r[i], s[j])
		}
	}
	step := 0.1 / math.Max(rawRate, isoRate)
	n := int(maxLag / step)
	offsets := make([]float64, 0, 2*n+1)
	scores := make([]float64, 0, 2*n+1)
	for k := -n; k <= n; k++ {
		o := guess + float64(k)*step
		var sum float64
		var used int
		for i := range r {
			// Position of this raw frame in the ISO window, in frames.
			pos := (rawStart + float64(i)/rawRate + o - isoStart) * isoRate
			j := int(math.Floor(pos))
			if j < 0 || j+1 >= len(s) {
				continue
			}
			frac := pos - float64(j)
			sum += sim[i][j]*(1-frac) + sim[i][j+1]*frac
			used++
		}
		if used*2 < len(r) {
			continue
		}
		offsets = append(offsets, o)
		scores = append(scores, sum/float64(used))
	}
	if len(scores) < 8 {
		return Peak{}, false
	}
	// Refine between the scored steps with a parabola through the peak.
	p := pick(offsets, scores, 0.25)
	for i := 1; i+1 < len(scores); i++ {
		if offsets[i] == p.Offset {
			a, b, c := scores[i-1], scores[i], scores[i+1]
			if den := a - 2*b + c; den < 0 {
				p.Offset += 0.5 * (a - c) / den * step
			}
		}
	}
	return p, true
}
