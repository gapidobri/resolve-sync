// Package atem reads the project file an ATEM ISO switcher writes next to its
// recordings and turns it into a program cut list.
//
// The file is line-delimited JSON. The first line is the full switcher state
// at the moment recording started; every later line is a timestamped delta.
package atem

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"resolve-sync/timebase"
)

// Source is one switcher input as listed in the project file.
type Source struct {
	Index int    // the number cuts refer to
	Name  string // label given on the switcher, e.g. "3 - center"
	Type  string // "Video", "Color", "ColorBars", "Still", ...
	// Files are the ISO recordings of this input in order. The switcher
	// starts a new file for every input partway through a long recording.
	Files []SourceFile
}

// SourceFile is one ISO recording.
type SourceFile struct {
	File  string // relative to the project folder
	Start int64  // its first frame, in frames since the recording started
}

// IsCamera reports whether the source is a recorded video input.
func (s Source) IsCamera() bool { return s.Type == "Video" && len(s.Files) > 0 }

// Segment is a stretch of program during which one source was on air.
// Frames count from the start of the recording at Recording.Rate.
type Segment struct {
	Source     int
	Start, End int64 // End is exclusive
}

// Transition is a gradual change between two sources. The surrounding
// segments meet at the midpoint; Start and End are when the switcher began
// and finished the move.
type Transition struct {
	Type       string // "Mix", "Dip", "Wipe", "DVE", "Sting"
	From, To   int
	Start, End int64
}

// Cut returns the frame where the two segments around the transition meet.
func (t Transition) Cut() int64 { return (t.Start + t.End) / 2 }

// Recording is a parsed project file.
type Recording struct {
	VideoMode   string       // as written by the switcher, e.g. "1080p30"
	Rate        timebase.Rat // frame rate implied by VideoMode; confirm against the ISO files
	StartTC     int64        // time of day of the first frame, in frames
	Duration    int64        // frames from the first to the last event in the file
	Sources     []Source
	Program     []Segment
	Transitions []Transition
	// Ignored counts switcher activity that is not translated, keyed by a
	// short description, so the caller can report it.
	Ignored map[string]int
}

// Source returns the source with the given index.
func (r *Recording) Source(index int) (Source, bool) {
	for _, s := range r.Sources {
		if s.Index == index {
			return s, true
		}
	}
	return Source{}, false
}

// Cameras returns the recorded video inputs in index order.
func (r *Recording) Cameras() []Source {
	var out []Source
	for _, s := range r.Sources {
		if s.IsCamera() {
			out = append(out, s)
		}
	}
	return out
}

var videoModeRE = regexp.MustCompile(`^(\d+)([pi])(\d+(?:\.\d+)?)$`)

// ParseVideoMode returns the frame rate of a mode string such as "1080p30",
// "1080p29.97" or "1080i50" (interlaced modes are given in fields per second).
func ParseVideoMode(mode string) (timebase.Rat, error) {
	m := videoModeRE.FindStringSubmatch(strings.TrimSpace(mode))
	if m == nil {
		return timebase.Rat{}, fmt.Errorf("unrecognised video mode %q", mode)
	}
	rate, err := timebase.ParseRate(m[3])
	if err != nil {
		return timebase.Rat{}, fmt.Errorf("unrecognised video mode %q", mode)
	}
	if m[2] == "i" {
		rate = rate.Div(timebase.Int(2))
	}
	return rate, nil
}

type rawSource struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	File          string `json:"file"`
	StartTimecode string `json:"startTimecode"`
	Index         int    `json:"_index_"`
}

type rawLine struct {
	Version         *int                         `json:"version"`
	MasterTimecode  string                       `json:"masterTimecode"`
	VideoMode       string                       `json:"videoMode"`
	Sources         []rawSource                  `json:"sources"`
	MixEffectBlocks []map[string]json.RawMessage `json:"mixEffectBlocks"`
	DownstreamKeys  []map[string]json.RawMessage `json:"downstreamKeys"`
}

// ParseFile reads a project file from disk.
func ParseFile(path string) (*Recording, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rec, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rec, nil
}

// Parse reads a project file.
func Parse(r io.Reader) (*Recording, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)

	rec := &Recording{Ignored: map[string]int{}}
	var (
		lineNo     int
		started    bool
		nominal    int64
		onAirSrc   int   // source currently on program
		segStart   int64 // where the current segment began
		transType  = "Mix"
		inTrans    bool
		transStart int64
		last       int64
	)
	closeSegment := func(at int64) {
		if at > segStart {
			rec.Program = append(rec.Program, Segment{Source: onAirSrc, Start: segStart, End: at})
		}
		segStart = at
	}

	for sc.Scan() {
		lineNo++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var ln rawLine
		if err := json.Unmarshal([]byte(text), &ln); err != nil {
			if !started && lineNo == 1 {
				return nil, fmt.Errorf("not an ATEM project file (a project saved by DaVinci Resolve has the same .drp extension but a different format): %w", err)
			}
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}

		if !started {
			if ln.VideoMode == "" || ln.MasterTimecode == "" || len(ln.MixEffectBlocks) == 0 {
				return nil, fmt.Errorf("line %d: not an ATEM project file: missing videoMode, masterTimecode or mixEffectBlocks", lineNo)
			}
			rate, err := ParseVideoMode(ln.VideoMode)
			if err != nil {
				return nil, err
			}
			rec.VideoMode, rec.Rate = ln.VideoMode, rate
			nominal = timebase.Nominal(rate)
			if rec.StartTC, err = timebase.ParseTimecode(ln.MasterTimecode, nominal); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			for _, s := range ln.Sources {
				src := Source{Index: s.Index, Name: s.Name, Type: s.Type}
				if s.File != "" {
					sf, err := sourceFile(s, rec.StartTC, nominal)
					if err != nil {
						return nil, fmt.Errorf("source %q: %w", s.Name, err)
					}
					src.Files = []SourceFile{sf}
				}
				rec.Sources = append(rec.Sources, src)
			}
			sort.Slice(rec.Sources, func(i, j int) bool { return rec.Sources[i].Index < rec.Sources[j].Index })

			me := ln.MixEffectBlocks[0]
			if err := unmarshalField(me, "source", &onAirSrc); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			_ = unmarshalField(me, "transitionType", &transType)
			_ = unmarshalField(me, "transitionActive", &inTrans)
			if len(ln.MixEffectBlocks) > 1 {
				rec.Ignored[fmt.Sprintf("%d additional mix effect blocks", len(ln.MixEffectBlocks)-1)]++
			}
			started = true
			continue
		}

		if ln.MasterTimecode == "" {
			return nil, fmt.Errorf("line %d: event without masterTimecode", lineNo)
		}
		tc, err := timebase.ParseTimecode(ln.MasterTimecode, nominal)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		at := wrap(tc-rec.StartTC, nominal)
		if at < last {
			// A recording crossing midnight more than once, or a corrupt file.
			return nil, fmt.Errorf("line %d: timecode %s goes backwards", lineNo, ln.MasterTimecode)
		}
		last = at

		if len(ln.DownstreamKeys) > 0 {
			rec.Ignored["downstream key changes"]++
		}
		// A sources entry on a later line means the switcher rolled over to
		// a new ISO file for that input.
		for _, s := range ln.Sources {
			if s.File == "" {
				continue
			}
			sf, err := sourceFile(s, rec.StartTC, nominal)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			for i := range rec.Sources {
				if rec.Sources[i].Index == s.Index {
					rec.Sources[i].Files = append(rec.Sources[i].Files, sf)
				}
			}
		}
		for i, me := range ln.MixEffectBlocks {
			idx := i
			_ = unmarshalField(me, "_index_", &idx)
			if idx != 0 {
				rec.Ignored["changes on mix effect block "+strconv.Itoa(idx+1)]++
				continue
			}
			var (
				src    *int
				active *bool
			)
			for key, val := range me {
				switch key {
				case "_index_", "transitionPosition", "onAir":
				case "source":
					src = new(int)
					if err := json.Unmarshal(val, src); err != nil {
						return nil, fmt.Errorf("line %d: source: %w", lineNo, err)
					}
				case "transitionActive":
					active = new(bool)
					if err := json.Unmarshal(val, active); err != nil {
						return nil, fmt.Errorf("line %d: transitionActive: %w", lineNo, err)
					}
				case "transitionType":
					_ = json.Unmarshal(val, &transType)
				case "fadeToBlack":
					rec.Ignored["fade to black"]++
				case "upstreamKeys":
					rec.Ignored["upstream key changes"]++
				default:
					rec.Ignored[key+" changes"]++
				}
			}

			switch {
			case active != nil && *active:
				// A transition begins; the new source arrives when it ends.
				inTrans, transStart = true, at
				if src != nil && *src != onAirSrc {
					closeSegment(at)
					onAirSrc = *src
				}
			case active != nil && !*active:
				if inTrans && src != nil && *src != onAirSrc {
					tr := Transition{Type: transType, From: onAirSrc, To: *src, Start: transStart, End: at}
					closeSegment(tr.Cut())
					onAirSrc = *src
					rec.Transitions = append(rec.Transitions, tr)
				} else if src != nil && *src != onAirSrc {
					closeSegment(at)
					onAirSrc = *src
				}
				// Otherwise the transition was aborted and nothing changed.
				inTrans = false
			case src != nil && *src != onAirSrc:
				closeSegment(at)
				onAirSrc = *src
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !started {
		return nil, fmt.Errorf("empty file")
	}
	rec.Duration = last
	closeSegment(last)
	return rec, nil
}

func sourceFile(s rawSource, startTC, nominal int64) (SourceFile, error) {
	sf := SourceFile{File: s.File}
	if s.StartTimecode != "" {
		tc, err := timebase.ParseTimecode(s.StartTimecode, nominal)
		if err != nil {
			return sf, err
		}
		sf.Start = wrap(tc-startTC, nominal)
	}
	return sf, nil
}

// wrap brings a frame difference into one day, for recordings that cross midnight.
func wrap(frames, nominal int64) int64 {
	day := 24 * 3600 * nominal
	return ((frames % day) + day) % day
}

func unmarshalField(m map[string]json.RawMessage, key string, into any) error {
	raw, ok := m[key]
	if !ok {
		return fmt.Errorf("missing %q", key)
	}
	return json.Unmarshal(raw, into)
}
