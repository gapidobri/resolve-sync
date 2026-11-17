// Package assemble combines a switcher cut list with synced camera footage
// into a multicam timeline, and reports everything that did not translate
// cleanly.
package assemble

import (
	"fmt"
	"sort"

	"resolve-sync/atem"
	"resolve-sync/fcpxml"
	"resolve-sync/media"
	"resolve-sync/timebase"
)

// NoSource marks an angle that was not connected to the switcher.
const NoSource = -1

// Angle is one camera's footage, already placed in time.
type Angle struct {
	Name   string
	Source int // switcher source index this camera fed, or NoSource
	Clips  []PlacedClip
}

// PlacedClip is a file with a known position relative to the recording.
type PlacedClip struct {
	Media media.Info
	// Offset is the time from the start of the switcher recording to the
	// clip's first frame. It is negative when the camera started first.
	Offset timebase.Rat
	// In and Out limit the placement to part of the file, in seconds from
	// its first frame; a zero Out means the end of the file. A long clip
	// whose clock drifts against the switcher is placed as several such
	// parts with slightly different offsets.
	In, Out timebase.Rat
}

func (c PlacedClip) out() timebase.Rat {
	if c.Out.IsZero() {
		return c.Media.Duration
	}
	return c.Out
}

// Options control the output.
type Options struct {
	Name string
	// Rate is the timeline frame rate; zero picks the rate most of the
	// footage was shot at.
	Rate timebase.Rat
	// ATEMRate overrides the rate implied by the project file's video mode,
	// for when the ISO files show the real rate differs (30 vs 29.97).
	ATEMRate timebase.Rat
	// AudioAngle is the angle whose audio plays throughout, or -1 for audio
	// to follow the picture.
	AudioAngle int
	// MinShotFrames removes shots shorter than this many timeline frames;
	// the shot before carries on in their place. Zero keeps everything that
	// is at least one frame long.
	MinShotFrames int64
}

// Span is a stretch of the timeline, in timeline frames.
type Span struct {
	Angle      string
	Start, End int64
}

// Report lists what the user should know about the result.
type Report struct {
	Rate      timebase.Rat
	Frames    int64
	Cuts      int
	Dissolves int
	// OnAirGaps are stretches where the switcher had a camera on air but
	// that camera has no footage; they are black in the timeline.
	OnAirGaps []Span
	// Unused are clips entirely outside the recording.
	Unused []string
	// Notes are everything else, in plain language.
	Notes []string
}

func (r *Report) notef(format string, a ...any) { r.Notes = append(r.Notes, fmt.Sprintf(format, a...)) }

// Timecode formats a timeline frame as elapsed time for display.
func (r *Report) Timecode(frame int64) string {
	return timebase.FormatTimecode(frame, timebase.Nominal(r.Rate))
}

// Build produces the timeline.
func Build(rec *atem.Recording, angles []Angle, opt Options) (*fcpxml.Project, *Report, error) {
	if len(angles) == 0 {
		return nil, nil, fmt.Errorf("no camera angles")
	}
	atemRate := opt.ATEMRate
	if atemRate.IsZero() {
		atemRate = rec.Rate
	}
	rate, width, height := opt.Rate, 0, 0
	mRate, mW, mH := majorityFormat(angles)
	if mRate.IsZero() {
		return nil, nil, fmt.Errorf("none of the selected footage contains video")
	}
	if rate.IsZero() {
		rate = mRate
	}
	width, height = mW, mH

	rep := &Report{Rate: rate}
	length := timebase.FrameTime(rec.Duration, atemRate) // seconds
	frames := length.Mul(rate).Floor()
	if frames <= 0 {
		return nil, nil, fmt.Errorf("the switcher recording is empty")
	}
	rep.Frames = frames
	atemToTimeline := func(f int64) int64 {
		return min(max(timebase.TimeFrame(timebase.FrameTime(f, atemRate), rate), 0), frames)
	}

	p := &fcpxml.Project{Name: opt.Name, Rate: rate, Width: width, Height: height, Frames: frames, AudioAngle: opt.AudioAngle}
	if p.Name == "" {
		p.Name = "ATEM"
	}
	if opt.AudioAngle < -1 || opt.AudioAngle >= len(angles) {
		return nil, nil, fmt.Errorf("audio angle %d does not exist", opt.AudioAngle)
	}

	// Place footage, trimmed to the recording window.
	bySource := map[int]int{}
	for ai, a := range angles {
		if a.Source != NoSource {
			if other, dup := bySource[a.Source]; dup {
				return nil, nil, fmt.Errorf("angles %q and %q are both assigned to switcher input %d", angles[other].Name, a.Name, a.Source)
			}
			bySource[a.Source] = ai
		}
		clips := append([]PlacedClip(nil), a.Clips...)
		sort.SliceStable(clips, func(i, j int) bool {
			return clips[i].Offset.Add(clips[i].In).Cmp(clips[j].Offset.Add(clips[j].In)) < 0
		})
		out := fcpxml.Angle{Name: a.Name}
		var at int64
		var prevPath string
		for _, c := range clips {
			start, end := c.Offset.Add(c.In), c.Offset.Add(c.out())
			// First and last timeline frames that fall inside the clip.
			f0 := max(start.Mul(rate).Ceil(), 0)
			f1 := min(end.Mul(rate).Floor(), frames)
			if f1 <= f0 {
				rep.Unused = append(rep.Unused, c.Media.Path)
				continue
			}
			if f0 < at {
				// Parts of one file are made to overlap on purpose so that
				// no frame is left empty between them.
				if c.Media.Path != prevPath {
					rep.notef("%s: %s overlaps the previous clip by %d frames; the overlap was trimmed from its start.", a.Name, c.Media.Name, at-f0)
				}
				f0 = at
				if f1 <= f0 {
					rep.Unused = append(rep.Unused, c.Media.Path)
					continue
				}
			}
			// Start on a whole frame of the source, and never run past its end.
			srcStart := timebase.FrameTime(f0, rate).Sub(c.Offset)
			if c.Media.HasVideo && !c.Media.Rate.IsZero() {
				srcStart = timebase.FrameTime(max(srcStart.Mul(c.Media.Rate).Round(), 0), c.Media.Rate)
			}
			for f1 > f0 && srcStart.Add(timebase.FrameTime(f1-f0, rate)).Cmp(c.Media.Duration) > 0 {
				f1--
			}
			if f1 <= f0 {
				rep.Unused = append(rep.Unused, c.Media.Path)
				continue
			}
			out.Clips = append(out.Clips, fcpxml.Clip{Media: c.Media, Offset: f0, Frames: f1 - f0, SrcStart: srcStart})
			at, prevPath = f1, c.Media.Path
		}
		if len(out.Clips) == 0 {
			rep.notef("%s: no footage inside the recording; the angle is empty.", a.Name)
		}
		p.Angles = append(p.Angles, out)
	}

	// Program cuts on the timeline grid.
	unmapped := map[int]int{}
	var tooShort int
	for _, seg := range rec.Program {
		start, end := atemToTimeline(seg.Start), atemToTimeline(seg.End)
		if end <= start {
			tooShort++ // shorter than one timeline frame
			continue
		}
		angle, ok := bySource[seg.Source]
		if !ok {
			angle = -1
			unmapped[seg.Source]++
		}
		if n := len(p.Cuts); n > 0 && p.Cuts[n-1].Angle == angle {
			p.Cuts[n-1].End = end
			continue
		}
		if n := len(p.Cuts); n > 0 {
			start = p.Cuts[n-1].End
		} else {
			start = 0
		}
		p.Cuts = append(p.Cuts, fcpxml.Cut{Angle: angle, Start: start, End: end})
	}
	if len(p.Cuts) == 0 {
		return nil, nil, fmt.Errorf("the project file contains no program")
	}
	p.Cuts[len(p.Cuts)-1].End = frames
	if removed := dropShortShots(p, opt.MinShotFrames); removed > 0 {
		rep.notef("%d shots shorter than %d frames were removed; the shot before each one carries on in its place.", removed, opt.MinShotFrames)
	}
	rep.Cuts = len(p.Cuts) - 1
	if tooShort > 0 {
		rep.notef("%d shots lasted less than one timeline frame (a brief slip on the switcher) and were dropped.", tooShort)
	}
	var srcIdx []int
	for s := range unmapped {
		srcIdx = append(srcIdx, s)
	}
	sort.Ints(srcIdx)
	for _, s := range srcIdx {
		name := fmt.Sprintf("input %d", s)
		if src, ok := rec.Source(s); ok {
			name = fmt.Sprintf("%q (%s)", src.Name, src.Type)
		}
		rep.notef("The switcher put %s on air %d times and no angle is assigned to it; those stretches are empty in the timeline.", name, unmapped[s])
	}

	// Dissolves, where both sides have picture and room for the overlap.
	cutAt := map[int64]int{} // boundary frame -> index of the cut starting there
	for i := 1; i < len(p.Cuts); i++ {
		cutAt[p.Cuts[i].Start] = i
	}
	used := map[int]int64{} // frames of each cut already claimed by a dissolve at its start
	var otherTypes, noRoom, noPicture int
	for _, tr := range rec.Transitions {
		if tr.Type != "Mix" {
			otherTypes++
			continue
		}
		at := atemToTimeline(tr.Cut())
		n := atemToTimeline(tr.End) - atemToTimeline(tr.Start)
		i, ok := cutAt[at]
		if !ok || n < 2 {
			continue // the cut vanished in rounding or merged with its neighbour
		}
		left, right := p.Cuts[i-1], p.Cuts[i]
		before, after := n/2, n-n/2
		if left.Angle < 0 || right.Angle < 0 ||
			!covered(p.Angles[left.Angle], at-before, at+after) || !covered(p.Angles[right.Angle], at-before, at+after) {
			noPicture++
			continue
		}
		if left.End-left.Start-used[i-1] < before || right.End-right.Start < after {
			noRoom++
			continue
		}
		used[i] = after
		p.Dissolves = append(p.Dissolves, fcpxml.Dissolve{At: at, Frames: n})
	}
	rep.Dissolves = len(p.Dissolves)
	if otherTypes > 0 {
		rep.notef("%d transitions were not mixes (wipe, dip, DVE or sting) and became hard cuts.", otherTypes)
	}
	if noPicture > 0 {
		rep.notef("%d dissolves became hard cuts because one of the two cameras has no footage at that moment.", noPicture)
	}
	if noRoom > 0 {
		rep.notef("%d dissolves became hard cuts because the shot next to them is shorter than the dissolve.", noRoom)
	}

	// Stretches on air without footage.
	for _, c := range p.Cuts {
		if c.Angle < 0 {
			continue
		}
		for _, g := range uncovered(p.Angles[c.Angle], c.Start, c.End) {
			if n := len(rep.OnAirGaps); n > 0 && rep.OnAirGaps[n-1].End == g.Start && rep.OnAirGaps[n-1].Angle == g.Angle {
				rep.OnAirGaps[n-1].End = g.End
				continue
			}
			rep.OnAirGaps = append(rep.OnAirGaps, g)
		}
	}

	var ignored []string
	for what := range rec.Ignored {
		ignored = append(ignored, what)
	}
	sort.Strings(ignored)
	for _, what := range ignored {
		rep.notef("Not translated: %s (%d).", what, rec.Ignored[what])
	}
	return p, rep, nil
}

// dropShortShots removes cuts shorter than minFrames and returns how many it
// removed. The previous shot is extended over each removed one (the next
// shot, for one at the very start), and shots of the same angle that end up
// side by side are joined.
func dropShortShots(p *fcpxml.Project, minFrames int64) int {
	if minFrames <= 1 || len(p.Cuts) < 2 {
		return 0
	}
	var kept []fcpxml.Cut
	removed := 0
	var lead int64 // frames of removed shots at the very start
	for _, c := range p.Cuts {
		switch {
		case c.End-c.Start >= minFrames:
			if len(kept) == 0 {
				c.Start -= lead
			}
			if n := len(kept); n > 0 && kept[n-1].Angle == c.Angle {
				kept[n-1].End = c.End
			} else {
				kept = append(kept, c)
			}
		case len(kept) == 0:
			lead += c.End - c.Start
			removed++
		default:
			kept[len(kept)-1].End = c.End
			removed++
		}
	}
	if len(kept) == 0 {
		return 0 // every shot is short; leave the program alone
	}
	p.Cuts = kept
	return removed
}

// majorityFormat returns the frame rate and frame size that the largest
// share of the footage, by running time, was shot at.
func majorityFormat(angles []Angle) (timebase.Rat, int, int) {
	type key struct {
		rate timebase.Rat
		w, h int
	}
	rateTime := map[timebase.Rat]float64{}
	sizeTime := map[key]float64{}
	for _, a := range angles {
		for _, c := range a.Clips {
			if !c.Media.HasVideo {
				continue
			}
			d := c.Media.Duration.Float()
			rateTime[c.Media.Rate] += d
			sizeTime[key{c.Media.Rate, c.Media.Width, c.Media.Height}] += d
		}
	}
	var best timebase.Rat
	for r, d := range rateTime {
		// Ties go to the lower rate so the result is deterministic.
		if best.IsZero() || d > rateTime[best] || (d == rateTime[best] && r.Cmp(best) < 0) {
			best = r
		}
	}
	var w, h int
	var bestD float64
	for k, d := range sizeTime {
		if k.rate == best && (d > bestD || (d == bestD && k.w*k.h > w*h)) {
			bestD, w, h = d, k.w, k.h
		}
	}
	return best, w, h
}

// uncovered returns the parts of [start, end) where the angle has no clip.
func uncovered(a fcpxml.Angle, start, end int64) []Span {
	var out []Span
	at := start
	for _, c := range a.Clips {
		cs, ce := c.Offset, c.Offset+c.Frames
		if ce <= at {
			continue
		}
		if cs >= end {
			break
		}
		if cs > at {
			out = append(out, Span{a.Name, at, cs})
		}
		at = ce
	}
	if at < end {
		out = append(out, Span{a.Name, at, end})
	}
	return out
}

func covered(a fcpxml.Angle, start, end int64) bool { return len(uncovered(a, start, end)) == 0 }
