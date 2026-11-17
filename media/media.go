// Package media describes source files. Probing shells out to ffprobe so the
// core has no cgo dependency; the path to the binary is configurable so a
// packaged app can point at its bundled copy.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"resolve-sync/timebase"
)

// Info is what the rest of the core needs to know about a file.
type Info struct {
	Path     string       // absolute
	Name     string       // file name without extension
	Rate     timebase.Rat // video frame rate; zero for audio-only files
	Frames   int64        // video frame count
	Duration timebase.Rat // seconds
	Width    int
	Height   int
	// TCStart is the embedded start timecode converted to seconds, or zero.
	// Editors address a file's frames starting from this value.
	TCStart       timebase.Rat
	Timecode      string // as embedded, for display
	HasVideo      bool
	HasAudio      bool
	AudioChannels int
	AudioRate     int
	Created       time.Time // camera clock at the start of the clip, if recorded
}

// Prober runs ffprobe.
type Prober struct {
	// FFprobe is the binary to run; empty means "ffprobe" from PATH.
	FFprobe string
}

type probeOutput struct {
	Streams []struct {
		CodecType    string            `json:"codec_type"`
		Width        int               `json:"width"`
		Height       int               `json:"height"`
		RFrameRate   string            `json:"r_frame_rate"`
		AvgFrameRate string            `json:"avg_frame_rate"`
		NbFrames     string            `json:"nb_frames"`
		Duration     string            `json:"duration"`
		Channels     int               `json:"channels"`
		SampleRate   string            `json:"sample_rate"`
		Tags         map[string]string `json:"tags"`
		Disposition  map[string]int    `json:"disposition"`
	} `json:"streams"`
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
}

// Probe reads a file's properties.
func (p Prober) Probe(ctx context.Context, path string) (Info, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Info{}, err
	}
	bin := p.FFprobe
	if bin == "" {
		bin = "ffprobe"
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", abs)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Info{}, fmt.Errorf("ffprobe %s: %w: %s", abs, err, strings.TrimSpace(stderr.String()))
	}
	info, err := parseProbe(stdout.Bytes())
	if err != nil {
		return Info{}, fmt.Errorf("%s: %w", abs, err)
	}
	info.Path = abs
	info.Name = strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	return info, nil
}

func parseProbe(data []byte) (Info, error) {
	var out probeOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return Info{}, fmt.Errorf("reading ffprobe output: %w", err)
	}
	var info Info
	timecode := out.Format.Tags["timecode"]
	for _, s := range out.Streams {
		if tc := s.Tags["timecode"]; tc != "" && (timecode == "" || s.CodecType == "video") {
			timecode = tc
		}
		switch s.CodecType {
		case "video":
			if info.HasVideo || s.Disposition["attached_pic"] == 1 {
				continue
			}
			rate, err := timebase.ParseRate(s.RFrameRate)
			if err != nil {
				if rate, err = timebase.ParseRate(s.AvgFrameRate); err != nil {
					return Info{}, fmt.Errorf("video stream has no usable frame rate")
				}
			}
			info.HasVideo, info.Rate, info.Width, info.Height = true, rate, s.Width, s.Height
			if n, err := strconv.ParseInt(s.NbFrames, 10, 64); err == nil && n > 0 {
				info.Frames = n
			} else if d, ok := parseSeconds(firstNonEmpty(s.Duration, out.Format.Duration)); ok {
				info.Frames = d.Mul(rate).Round()
			}
		case "audio":
			if info.HasAudio {
				continue
			}
			info.HasAudio, info.AudioChannels = true, s.Channels
			info.AudioRate, _ = strconv.Atoi(s.SampleRate)
		}
	}
	if !info.HasVideo && !info.HasAudio {
		return Info{}, fmt.Errorf("no video or audio stream")
	}
	if info.HasVideo && info.Frames > 0 {
		info.Duration = timebase.FrameTime(info.Frames, info.Rate)
	} else if d, ok := parseSeconds(out.Format.Duration); ok {
		info.Duration = d
	} else {
		return Info{}, fmt.Errorf("unknown duration")
	}
	if timecode != "" && info.HasVideo {
		if frames, err := timebase.ParseTimecode(timecode, timebase.Nominal(info.Rate)); err == nil {
			info.Timecode = timecode
			info.TCStart = timebase.FrameTime(frames, info.Rate)
		}
	}
	if ct := out.Format.Tags["creation_time"]; ct != "" {
		if t, err := time.Parse(time.RFC3339Nano, ct); err == nil {
			info.Created = t
		}
	}
	return info, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" && s != "N/A" {
			return s
		}
	}
	return ""
}

// parseSeconds converts a decimal seconds string to a rational, to the microsecond.
func parseSeconds(s string) (timebase.Rat, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return timebase.Rat{}, false
	}
	return timebase.New(int64(f*1e6+0.5), 1e6), true
}
