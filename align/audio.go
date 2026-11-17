package align

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"os/exec"
	"strconv"
	"strings"
)

// Audio decodes a stretch of a file's first audio stream to mono samples at
// the given rate. Audio decodes hundreds of times faster than picture.
func (d Decoder) Audio(ctx context.Context, path string, start, dur float64, rate int) ([]float32, error) {
	bin := d.FFmpeg
	if bin == "" {
		bin = "ffmpeg"
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-v", "error", "-nostdin",
		"-ss", secs(start), "-t", secs(dur), "-i", path, "-vn",
		"-ac", "1", "-ar", strconv.Itoa(rate), "-f", "s16le", "-")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	raw := stdout.Bytes()
	out := make([]float32, len(raw)/2)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(raw[2*i:]))) / 32768
	}
	return out, nil
}

// Envelope reduces audio to its loudness contour: one value per hop samples,
// the log of the energy in that hop, with its slow trend removed. Two
// microphones in the same room have very different waveforms but nearly the
// same contour, and it is short enough to compare across hours. The trend
// over win hops either side is removed so that the difference between quiet
// and loud passages does not dominate.
func Envelope(samples []float32, hop, win int) []float32 {
	n := len(samples) / hop
	env := make([]float32, n)
	for i := range env {
		var e float64
		for _, v := range samples[i*hop : (i+1)*hop] {
			e += float64(v) * float64(v)
		}
		env[i] = float32(math.Log(e/float64(hop) + 1e-9))
	}
	return detrend(env, win)
}

func detrend(env []float32, win int) []float32 {
	n := len(env)
	out := make([]float32, n)
	var sum float64
	lo, hi := 0, 0
	for i := range env {
		for hi < n && hi <= i+win {
			sum += float64(env[hi])
			hi++
		}
		for lo < i-win {
			sum -= float64(env[lo])
			lo++
		}
		out[i] = env[i] - float32(sum/float64(hi-lo))
	}
	return out
}

// Loudness returns the loudness contour of a whole file without holding its
// audio in memory: one value per hop samples at the given sample rate, with
// its trend removed (for matching), and the level of each second (for
// display; see Waveform).
func (d Decoder) Loudness(ctx context.Context, path string, rate, hop, win int) ([]float32, Waveform, error) {
	bin := d.FFmpeg
	if bin == "" {
		bin = "ffmpeg"
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-v", "error", "-nostdin", "-i", path, "-vn",
		"-ac", "1", "-ar", strconv.Itoa(rate), "-f", "s16le", "-")
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	var env []float32
	buf := make([]byte, 2*hop)
	r := bufio.NewReaderSize(stdout, 1<<20)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		var e float64
		for i := 0; i < len(buf); i += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(buf[i:]))) / 32768
			e += v * v
		}
		env = append(env, float32(math.Log(e/float64(hop)+1e-9)))
	}
	if err := cmd.Wait(); err != nil {
		return nil, nil, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return detrend(env, win), waveform(env, rate/hop), nil
}

// Waveform is the loudest level of each second of a recording, on a scale
// of 0 (-60 dBFS or quieter) to 255 (full scale).
type Waveform []byte

// waveform reduces log-energy values, perSecond of them a second, to a
// Waveform.
func waveform(env []float32, perSecond int) Waveform {
	if perSecond <= 0 {
		return nil
	}
	out := make(Waveform, 0, len(env)/perSecond+1)
	for i := 0; i < len(env); i += perSecond {
		peak := float32(-100)
		for _, v := range env[i:min(i+perSecond, len(env))] {
			peak = max(peak, v)
		}
		db := 10 / math.Ln10 * float64(peak) // log energy to dB
		out = append(out, byte(math.Round(255*math.Min(1, math.Max(0, (db+60)/60)))))
	}
	return out
}

func fft(a []complex128, invert bool) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		ang := 2 * math.Pi / float64(size)
		if invert {
			ang = -ang
		}
		wl := cmplx.Rect(1, ang)
		for i := 0; i < n; i += size {
			w := complex(1, 0)
			for k := 0; k < size/2; k++ {
				u, v := a[i+k], a[i+k+size/2]*w
				a[i+k], a[i+k+size/2] = u+v, u-v
				w *= wl
			}
		}
	}
	if invert {
		for i := range a {
			a[i] /= complex(float64(n), 0)
		}
	}
}

// Correlate finds the lag at which b best lines up with a: the returned
// Offset is in samples, and means that b[i] corresponds to a[i+Offset].
//
// With whiten set, every frequency counts equally (phase transform), which
// gives a much sharper peak for waveforms from different microphones.
// exclude is how many samples around the peak count as the same answer when
// finding the runner-up.
func Correlate(a, b []float32, whiten bool, exclude int) (Peak, bool) {
	if len(a) < 16 || len(b) < 16 {
		return Peak{}, false
	}
	n := 1
	for n < len(a)+len(b) {
		n <<= 1
	}
	fa, fb := make([]complex128, n), make([]complex128, n)
	var ea, eb float64
	for i, v := range a {
		fa[i] = complex(float64(v), 0)
		ea += float64(v) * float64(v)
	}
	for i, v := range b {
		fb[i] = complex(float64(v), 0)
		eb += float64(v) * float64(v)
	}
	if ea == 0 || eb == 0 {
		return Peak{}, false
	}
	fft(fa, false)
	fft(fb, false)
	for i := range fa {
		c := fa[i] * cmplx.Conj(fb[i])
		if whiten {
			if m := cmplx.Abs(c); m > 1e-12 {
				c /= complex(m, 0)
			}
		}
		fa[i] = c
	}
	fft(fa, true)
	// fa[k] is the correlation at lag k for k >= 0 and k-n for negative lags.
	scale := 1 / math.Sqrt(ea*eb)
	if whiten {
		scale = 1
	}
	lags := len(a) + len(b) - 1
	offsets := make([]float64, 0, lags)
	scores := make([]float64, 0, lags)
	for lag := -(len(b) - 1); lag < len(a); lag++ {
		k := lag
		if k < 0 {
			k += n
		}
		offsets = append(offsets, float64(lag))
		scores = append(scores, real(fa[k])*scale)
	}
	return pick(offsets, scores, float64(exclude)), true
}
