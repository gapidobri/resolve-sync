// Package fcpxml writes a multicam clip and a timeline that cuts between its
// angles, in the FCPXML dialect DaVinci Resolve imports as a true multicam
// clip. The structure mirrors spike/2-full.fcpxml, which was verified by hand.
package fcpxml

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"strings"

	"resolve-sync/media"
	"resolve-sync/timebase"
)

// Project is a timeline at one frame rate. All frame values are in that rate
// and count from the start of the timeline.
type Project struct {
	Name          string
	Rate          timebase.Rat
	Width, Height int
	Frames        int64 // total length
	Angles        []Angle
	// Cuts tile [0, Frames) in order.
	Cuts      []Cut
	Dissolves []Dissolve
	// AudioAngle is the angle whose audio plays throughout, or -1 for audio
	// to follow the picture.
	AudioAngle int
}

// Angle is one camera. Clips are in order and do not overlap; the stretches
// between them are empty.
type Angle struct {
	Name  string
	Clips []Clip
}

// Clip places part of a file in an angle.
type Clip struct {
	Media    media.Info
	Offset   int64        // where the clip starts in the timeline
	Frames   int64        // how long it runs, in timeline frames
	SrcStart timebase.Rat // seconds into the file where it starts
}

// Cut is a stretch of the timeline showing one angle.
type Cut struct {
	Angle      int // index into Angles, or -1 for nothing
	Start, End int64
}

// Dissolve is a cross dissolve centred on the boundary between two cuts.
type Dissolve struct {
	At     int64 // the boundary frame
	Frames int64 // total length
}

// Validate reports structural problems that would produce a broken file.
func (p *Project) Validate() error {
	if p.Rate.N <= 0 || p.Frames <= 0 {
		return fmt.Errorf("fcpxml: project needs a frame rate and a length")
	}
	if p.AudioAngle < -1 || p.AudioAngle >= len(p.Angles) {
		return fmt.Errorf("fcpxml: audio angle %d out of range", p.AudioAngle)
	}
	for ai, a := range p.Angles {
		var at int64
		for ci, c := range a.Clips {
			if c.Frames <= 0 || c.Offset < at || c.Offset+c.Frames > p.Frames {
				return fmt.Errorf("fcpxml: angle %q clip %d (%s) is empty, overlaps the previous clip or leaves the timeline", a.Name, ci, c.Media.Name)
			}
			at = c.Offset + c.Frames
		}
		_ = ai
	}
	var at int64
	bounds := map[int64]bool{}
	for i, c := range p.Cuts {
		if c.Start != at || c.End <= c.Start || c.Angle < -1 || c.Angle >= len(p.Angles) {
			return fmt.Errorf("fcpxml: cut %d (%d-%d, angle %d) does not follow the previous cut", i, c.Start, c.End, c.Angle)
		}
		if i > 0 {
			bounds[c.Start] = true
		}
		at = c.End
	}
	if at != p.Frames {
		return fmt.Errorf("fcpxml: cuts end at frame %d but the timeline is %d long", at, p.Frames)
	}
	for _, d := range p.Dissolves {
		if !bounds[d.At] || d.Frames <= 0 {
			return fmt.Errorf("fcpxml: dissolve at frame %d is not on a cut", d.At)
		}
	}
	return nil
}

type formatKey struct {
	w, h int
	rate timebase.Rat
}

// formatName returns Final Cut's name for common formats; importers match on
// it when present.
func formatName(k formatKey) string {
	rates := map[timebase.Rat]string{
		timebase.New(24000, 1001): "2398", timebase.Int(24): "24", timebase.Int(25): "25",
		timebase.New(30000, 1001): "2997", timebase.Int(30): "30", timebase.Int(50): "50",
		timebase.New(60000, 1001): "5994", timebase.Int(60): "60",
	}
	r, ok := rates[k.rate]
	if !ok {
		return ""
	}
	switch {
	case k.w == 1920 && k.h == 1080:
		return "FFVideoFormat1080p" + r
	case k.w == 1280 && k.h == 720:
		return "FFVideoFormat720p" + r
	case k.w == 3840 && k.h == 2160, k.w == 4096 && k.h == 2160:
		return fmt.Sprintf("FFVideoFormat%dx%dp%s", k.w, k.h, r)
	}
	return ""
}

type writer struct {
	w   *bufio.Writer
	ind int
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// open writes a start tag; attrs are name, value pairs. Empty values are skipped.
func (w *writer) tag(selfClose bool, name string, attrs ...string) {
	w.w.WriteString(strings.Repeat("  ", w.ind))
	w.w.WriteString("<" + name)
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i+1] != "" {
			fmt.Fprintf(w.w, ` %s="%s"`, attrs[i], esc(attrs[i+1]))
		}
	}
	if selfClose {
		w.w.WriteString("/>\n")
	} else {
		w.w.WriteString(">\n")
		w.ind++
	}
}
func (w *writer) empty(name string, attrs ...string) { w.tag(true, name, attrs...) }
func (w *writer) open(name string, attrs ...string)  { w.tag(false, name, attrs...) }
func (w *writer) close(name string) {
	w.ind--
	w.w.WriteString(strings.Repeat("  ", w.ind) + "</" + name + ">\n")
}

func fileURL(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }

func boolAttr(b bool) string {
	if b {
		return "1"
	}
	return ""
}

// Write emits the project.
func Write(out io.Writer, p *Project) error {
	if err := p.Validate(); err != nil {
		return err
	}
	w := &writer{w: bufio.NewWriter(out)}
	t := func(frames int64) string { return timebase.FrameTime(frames, p.Rate).String() }

	// Resource ids: the timeline format first, then one format per distinct
	// source format, one asset per file, and the multicam itself.
	next := 0
	newID := func() string { next++; return fmt.Sprintf("r%d", next) }
	timelineFmt := formatKey{p.Width, p.Height, p.Rate}
	formats := map[formatKey]string{timelineFmt: newID()}
	formatOrder := []formatKey{timelineFmt}
	assets := map[string]string{}
	var assetOrder []media.Info
	for _, a := range p.Angles {
		for _, c := range a.Clips {
			if _, ok := assets[c.Media.Path]; ok {
				continue
			}
			if c.Media.HasVideo {
				k := formatKey{c.Media.Width, c.Media.Height, c.Media.Rate}
				if _, ok := formats[k]; !ok {
					formats[k] = newID()
					formatOrder = append(formatOrder, k)
				}
			}
			assets[c.Media.Path] = newID()
			assetOrder = append(assetOrder, c.Media)
		}
	}
	multicamID := newID()
	multicamName := p.Name + " Multicam"
	angleID := func(i int) string { return fmt.Sprintf("angle%d", i+1) }

	w.w.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE fcpxml>\n")
	w.open("fcpxml", "version", "1.9")
	w.open("resources")
	for _, k := range formatOrder {
		w.empty("format", "id", formats[k], "name", formatName(k),
			"frameDuration", timebase.Int(1).Div(k.rate).String(),
			"width", fmt.Sprint(k.w), "height", fmt.Sprint(k.h))
	}
	for _, m := range assetOrder {
		attrs := []string{"id", assets[m.Path], "name", m.Name,
			"start", m.TCStart.String(), "duration", m.Duration.String(),
			"hasVideo", boolAttr(m.HasVideo), "hasAudio", boolAttr(m.HasAudio)}
		if m.HasVideo {
			attrs = append(attrs, "format", formats[formatKey{m.Width, m.Height, m.Rate}], "videoSources", "1")
		}
		if m.HasAudio {
			attrs = append(attrs, "audioSources", "1", "audioChannels", fmt.Sprint(m.AudioChannels), "audioRate", fmt.Sprint(m.AudioRate))
		}
		w.open("asset", attrs...)
		w.empty("media-rep", "kind", "original-media", "src", fileURL(m.Path))
		w.close("asset")
	}
	w.open("media", "id", multicamID, "name", multicamName)
	w.open("multicam", "format", formats[timelineFmt], "tcStart", "0s", "tcFormat", "NDF")
	for i, a := range p.Angles {
		w.open("mc-angle", "name", a.Name, "angleID", angleID(i))
		var at int64
		for _, c := range a.Clips {
			if c.Offset > at {
				w.empty("gap", "name", "Gap", "offset", t(at), "start", "0s", "duration", t(c.Offset-at))
			}
			format := "" // audio-only clips carry no picture format
			if c.Media.HasVideo {
				format = formats[formatKey{c.Media.Width, c.Media.Height, c.Media.Rate}]
			}
			w.empty("asset-clip", "ref", assets[c.Media.Path], "name", c.Media.Name,
				"offset", t(c.Offset), "start", c.Media.TCStart.Add(c.SrcStart).String(),
				"duration", t(c.Frames), "format", format, "tcFormat", "NDF")
			at = c.Offset + c.Frames
		}
		w.close("mc-angle")
	}
	w.close("multicam")
	w.close("media")
	w.close("resources")

	dissolves := map[int64]int64{}
	for _, d := range p.Dissolves {
		dissolves[d.At] = d.Frames
	}
	w.open("library")
	w.open("event", "name", p.Name)
	w.open("project", "name", p.Name)
	w.open("sequence", "format", formats[timelineFmt], "duration", t(p.Frames),
		"tcStart", "0s", "tcFormat", "NDF", "audioLayout", "stereo", "audioRate", "48k")
	w.open("spine")
	for i, c := range p.Cuts {
		if n, ok := dissolves[c.Start]; ok && i > 0 {
			// Offsets are in whole frames; an odd length leans one frame late.
			w.empty("transition", "name", "Cross Dissolve", "offset", t(c.Start-n/2), "duration", t(n))
		}
		if c.Angle < 0 {
			w.empty("gap", "name", "Gap", "offset", t(c.Start), "start", "0s", "duration", t(c.End-c.Start))
			continue
		}
		w.open("mc-clip", "ref", multicamID, "name", multicamName,
			"offset", t(c.Start), "start", t(c.Start), "duration", t(c.End-c.Start))
		if p.AudioAngle < 0 || p.AudioAngle == c.Angle {
			w.empty("mc-source", "angleID", angleID(c.Angle), "srcEnable", "all")
		} else {
			w.empty("mc-source", "angleID", angleID(c.Angle), "srcEnable", "video")
			w.empty("mc-source", "angleID", angleID(p.AudioAngle), "srcEnable", "audio")
		}
		w.close("mc-clip")
	}
	w.close("spine")
	w.close("sequence")
	w.close("project")
	w.close("event")
	w.close("library")
	w.close("fcpxml")
	return w.w.Flush()
}
