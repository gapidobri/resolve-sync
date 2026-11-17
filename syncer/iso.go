// Package syncer places raw camera footage on the switcher's timeline.
//
// It works in three stages, cheapest first:
//
//  1. Sound: every clip's loudness contour is compared with the others, which
//     places all clips relative to one another within a fraction of a second.
//     This costs seconds, and is the only way to place a camera that was not
//     connected to the switcher.
//  2. Anchor: one picture search against the ISO recordings fixes where that
//     whole group sits on the switcher's clock.
//  3. Picture: short windows of every clip are matched frame by frame against
//     the camera's ISO recording. This is what makes the result frame
//     accurate; sound alone is late by about 3 ms per metre between a camera
//     and the stage.
//
// Nothing here writes to the footage: files are only read through ffmpeg.
package syncer

import (
	"context"
	"fmt"
	"path/filepath"

	"resolve-sync/atem"
	"resolve-sync/media"
	"resolve-sync/timebase"
)

// ISOFile is one ISO recording with its position on the switcher's clock.
type ISOFile struct {
	Media media.Info
	Start timebase.Rat // seconds from the start of the recording
}

// ISOSet is the ISO recordings of every switcher input.
type ISOSet struct {
	// Rate is the switcher's real frame rate, taken from the files.
	Rate  timebase.Rat
	Files map[int][]ISOFile // by source index, in order
}

// LoadISO probes the ISO recordings named in the project file.
func LoadISO(ctx context.Context, prober media.Prober, rec *atem.Recording, dir string) (*ISOSet, error) {
	set := &ISOSet{Rate: rec.Rate, Files: map[int][]ISOFile{}}
	type probed struct {
		src   int
		info  media.Info
		start int64
	}
	var all []probed
	for _, cam := range rec.Cameras() {
		for _, sf := range cam.Files {
			info, err := prober.Probe(ctx, filepath.Join(dir, filepath.FromSlash(sf.File)))
			if err != nil {
				return nil, fmt.Errorf("input %d (%s): %w", cam.Index, cam.Name, err)
			}
			if !info.HasVideo {
				return nil, fmt.Errorf("input %d (%s): %s has no video", cam.Index, cam.Name, sf.File)
			}
			// The files are the authority on the real rate (30 vs 29.97).
			if timebase.Nominal(info.Rate) == timebase.Nominal(rec.Rate) {
				set.Rate = info.Rate
			}
			all = append(all, probed{cam.Index, info, sf.Start})
		}
	}
	startTC := timebase.FrameTime(rec.StartTC, set.Rate)
	day := timebase.Int(24 * 3600 * timebase.Nominal(set.Rate)).Div(set.Rate)
	for _, p := range all {
		// Prefer the timecode embedded in the file over the project file's
		// value: the two can disagree by a frame where the switcher rolled
		// over to a new file.
		start := timebase.FrameTime(p.start, set.Rate)
		if p.info.Timecode != "" {
			start = p.info.TCStart.Sub(startTC)
			if start.Cmp(timebase.Int(0)) < 0 {
				start = start.Add(day)
			}
		}
		set.Files[p.src] = append(set.Files[p.src], ISOFile{Media: p.info, Start: start})
	}
	return set, nil
}

// locate returns the file that holds the whole stretch [t, t+dur) of an
// input, in seconds on the switcher's clock.
func (s *ISOSet) locate(src int, t, dur float64) (ISOFile, bool) {
	for _, f := range s.Files[src] {
		start := f.Start.Float()
		if t >= start && t+dur <= start+f.Media.Duration.Float() {
			return f, true
		}
	}
	return ISOFile{}, false
}

// span returns the first and last moment an input was recorded.
func (s *ISOSet) span(src int) (float64, float64) {
	files := s.Files[src]
	if len(files) == 0 {
		return 0, 0
	}
	last := files[len(files)-1]
	return files[0].Start.Float(), last.Start.Float() + last.Media.Duration.Float()
}
