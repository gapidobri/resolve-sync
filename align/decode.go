// Package align finds where a raw camera clip sits relative to the
// switcher's ISO recording of the same camera, by comparing pictures.
//
// Decoding shells out to ffmpeg and only ever reads the footage: every
// invocation writes raw frames to a pipe and nothing to disk.
package align

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"resolve-sync/timebase"
)

// Decoder runs ffmpeg.
type Decoder struct {
	// FFmpeg is the binary to run; empty means "ffmpeg" from PATH.
	FFmpeg string
	// BRAW is the helper that decodes Blackmagic RAW picture, which ffmpeg
	// cannot; empty means such clips have no decodable picture.
	BRAW string
}

func isBRAW(path string) bool { return strings.EqualFold(filepath.Ext(path), ".braw") }

// braw runs the Blackmagic RAW helper: count frames from first, every step.
func (d Decoder) braw(ctx context.Context, path string, first, count, step int64, w, h int) ([]Frame, error) {
	if d.BRAW == "" {
		return nil, fmt.Errorf("no Blackmagic RAW decoder available")
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, d.BRAW, path,
		strconv.FormatInt(first, 10), strconv.FormatInt(count, 10), strconv.FormatInt(step, 10),
		strconv.Itoa(w), strconv.Itoa(h))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("brawframes: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return split(stdout.Bytes(), w*h), nil
}

func split(raw []byte, size int) []Frame {
	frames := make([]Frame, 0, len(raw)/size)
	for off := 0; off+size <= len(raw); off += size {
		f := make(Frame, size)
		for i, b := range raw[off : off+size] {
			f[i] = float32(b)
		}
		frames = append(frames, f)
	}
	return frames
}

// Frame is a small grayscale picture, row by row.
type Frame []float32

func (d Decoder) run(ctx context.Context, w, h int, args ...string) ([]Frame, error) {
	bin := d.FFmpeg
	if bin == "" {
		bin = "ffmpeg"
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, append([]string{"-v", "error", "-nostdin"}, args...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return split(stdout.Bytes(), w*h), nil
}

func secs(t float64) string { return strconv.FormatFloat(t, 'f', 6, 64) }

func scaleFilter(w, h int) string {
	return fmt.Sprintf("scale=%d:%d:flags=area,format=gray", w, h)
}

// Track samples a stretch of a file at a low, even rate using keyframes
// only, which is many times faster than decoding everything. Sample i shows
// the picture at roughly start + i/fps; the error is up to one keyframe
// interval, so a track is only good for coarse placement.
func (d Decoder) Track(ctx context.Context, path string, start, dur, fps float64, w, h int) ([]Frame, error) {
	return d.run(ctx, w, h,
		"-skip_frame", "nokey", "-ss", secs(start), "-t", secs(dur), "-i", path, "-an",
		"-vf", fmt.Sprintf("fps=%s,%s", secs(fps), scaleFilter(w, h)), "-f", "rawvideo", "-")
}

// Thumbnail returns one frame, at the given time into the file, as a small
// colour picture of width w (the height follows the picture's shape).
func (d Decoder) Thumbnail(ctx context.Context, path string, rate timebase.Rat, at float64, w, h int) (*image.RGBA, error) {
	var raw []byte
	if isBRAW(path) {
		if d.BRAW == "" {
			return nil, fmt.Errorf("no Blackmagic RAW decoder available")
		}
		frame := int64(at * rate.Float())
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, d.BRAW, path, strconv.FormatInt(frame, 10), "1", "1", strconv.Itoa(w), strconv.Itoa(h), "rgb")
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("brawframes: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		raw = stdout.Bytes()
	} else {
		bin := d.FFmpeg
		if bin == "" {
			bin = "ffmpeg"
		}
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, "-v", "error", "-nostdin", "-ss", secs(at), "-i", path, "-an",
			"-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:%d:flags=area,format=rgb24", w, h), "-f", "rawvideo", "-")
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		raw = stdout.Bytes()
	}
	if len(raw) < w*h*3 {
		return nil, fmt.Errorf("no picture at %.1f s", at)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		img.Pix[4*i], img.Pix[4*i+1], img.Pix[4*i+2], img.Pix[4*i+3] = raw[3*i], raw[3*i+1], raw[3*i+2], 255
	}
	return img, nil
}

// Window decodes n consecutive frames starting at the given frame number.
func (d Decoder) Window(ctx context.Context, path string, rate timebase.Rat, first int64, n, w, h int) ([]Frame, error) {
	if first < 0 {
		first = 0
	}
	if isBRAW(path) {
		return d.braw(ctx, path, first, int64(n), 1, w, h)
	}
	// Seeking to half a frame before the wanted one makes it the first frame out.
	start := (float64(first) - 0.5) / rate.Float()
	if start < 0 {
		start = 0
	}
	return d.run(ctx, w, h,
		"-ss", secs(start), "-i", path, "-an", "-frames:v", strconv.Itoa(n),
		"-vf", scaleFilter(w, h), "-f", "rawvideo", "-")
}
