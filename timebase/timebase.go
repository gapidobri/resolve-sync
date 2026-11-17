// Package timebase provides exact rational time arithmetic and timecode
// parsing. Everything in the core is expressed either as integer frames at a
// known rate or as rational seconds, never as floats.
package timebase

import (
	"fmt"
	"strconv"
	"strings"
)

// Rat is a rational number with a positive denominator, kept in lowest terms.
// It is used both for times in seconds and for frame rates in frames/second.
type Rat struct{ N, D int64 }

func gcd(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// New returns n/d in lowest terms. It panics on a zero denominator.
func New(n, d int64) Rat {
	if d == 0 {
		panic("timebase: zero denominator")
	}
	if d < 0 {
		n, d = -n, -d
	}
	if g := gcd(n, d); g > 1 {
		n, d = n/g, d/g
	}
	return Rat{n, d}
}

// Int returns n as a rational.
func Int(n int64) Rat { return Rat{n, 1} }

func (a Rat) norm() Rat {
	if a.D == 0 {
		return Rat{0, 1}
	}
	return a
}

func (a Rat) Add(b Rat) Rat { a, b = a.norm(), b.norm(); return New(a.N*b.D+b.N*a.D, a.D*b.D) }
func (a Rat) Sub(b Rat) Rat { a, b = a.norm(), b.norm(); return New(a.N*b.D-b.N*a.D, a.D*b.D) }
func (a Rat) Mul(b Rat) Rat {
	a, b = a.norm(), b.norm()
	// Cross-reduce first to keep intermediates small.
	g1, g2 := gcd(a.N, b.D), gcd(b.N, a.D)
	if g1 == 0 {
		g1 = 1
	}
	if g2 == 0 {
		g2 = 1
	}
	return New((a.N/g1)*(b.N/g2), (a.D/g2)*(b.D/g1))
}
func (a Rat) Div(b Rat) Rat { b = b.norm(); return a.Mul(Rat{b.D, b.N}.fixSign()) }

func (a Rat) fixSign() Rat {
	if a.D < 0 {
		return Rat{-a.N, -a.D}
	}
	return a
}

// Cmp returns -1, 0 or 1.
func (a Rat) Cmp(b Rat) int {
	d := a.Sub(b).N
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}

func (a Rat) IsZero() bool   { return a.N == 0 }
func (a Rat) Float() float64 { a = a.norm(); return float64(a.N) / float64(a.D) }

// Round returns the nearest integer, halves rounded away from zero.
func (a Rat) Round() int64 {
	a = a.norm()
	n, d := a.N, a.D
	if n >= 0 {
		return (2*n + d) / (2 * d)
	}
	return -((-2*n + d) / (2 * d))
}

// Floor returns the largest integer not above a.
func (a Rat) Floor() int64 {
	a = a.norm()
	q := a.N / a.D
	if a.N%a.D != 0 && a.N < 0 {
		q--
	}
	return q
}

// Ceil returns the smallest integer not below a.
func (a Rat) Ceil() int64 { return -Rat{-a.norm().N, a.norm().D}.Floor() }

// String formats the value the way FCPXML expects a time: "5s" or "19/2s".
func (a Rat) String() string {
	a = a.norm()
	if a.D == 1 {
		return strconv.FormatInt(a.N, 10) + "s"
	}
	return fmt.Sprintf("%d/%ds", a.N, a.D)
}

// FrameTime converts a frame count at the given rate to seconds.
func FrameTime(frames int64, rate Rat) Rat { return Int(frames).Div(rate) }

// TimeFrame converts seconds to the nearest frame at the given rate.
func TimeFrame(t Rat, rate Rat) int64 { return t.Mul(rate).Round() }

// ParseRate parses "30000/1001", "25/1", "25" or "29.97" into a frame rate.
// Decimal values close to the NTSC rates are mapped to their exact fractions.
func ParseRate(s string) (Rat, error) {
	s = strings.TrimSpace(s)
	if num, den, ok := strings.Cut(s, "/"); ok {
		n, err1 := strconv.ParseInt(num, 10, 64)
		d, err2 := strconv.ParseInt(den, 10, 64)
		if err1 != nil || err2 != nil || d == 0 || n <= 0 {
			return Rat{}, fmt.Errorf("invalid frame rate %q", s)
		}
		return New(n, d), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return Rat{}, fmt.Errorf("invalid frame rate %q", s)
	}
	if f == float64(int64(f)) {
		return Int(int64(f)), nil
	}
	nominal := int64(f + 0.5)
	if ntsc := float64(nominal) * 1000 / 1001; f > ntsc-0.01 && f < ntsc+0.01 {
		return New(nominal*1000, 1001), nil
	}
	return New(int64(f*1000+0.5), 1000), nil
}

// Nominal returns the timecode base of a rate: 30 for 29.97, 24 for 23.976.
func Nominal(rate Rat) int64 { return rate.Round() }

// ParseTimecode parses "HH:MM:SS:FF" (or "HH:MM:SS;FF" for drop-frame) into a
// count of real frames since 00:00:00:00, for a timecode base of nominal
// frames per second.
func ParseTimecode(s string, nominal int64) (int64, error) {
	if nominal <= 0 {
		return 0, fmt.Errorf("invalid timecode base %d", nominal)
	}
	drop := strings.ContainsAny(s, ";.")
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == ';' || r == '.' })
	if len(parts) != 4 {
		return 0, fmt.Errorf("invalid timecode %q", s)
	}
	var v [4]int64
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid timecode %q", s)
		}
		v[i] = n
	}
	h, m, sec, f := v[0], v[1], v[2], v[3]
	if m > 59 || sec > 59 || f >= nominal {
		return 0, fmt.Errorf("invalid timecode %q at %d fps", s, nominal)
	}
	frames := ((h*60+m)*60+sec)*nominal + f
	if drop {
		// Drop-frame skips 2 frame numbers (4 at 60) every minute except each tenth.
		dropPerMin := nominal / 15
		totalMin := h*60 + m
		frames -= dropPerMin * (totalMin - totalMin/10)
	}
	return frames, nil
}

// FormatTimecode renders a non-drop timecode for display.
func FormatTimecode(frames, nominal int64) string {
	if frames < 0 {
		return "-" + FormatTimecode(-frames, nominal)
	}
	f := frames % nominal
	s := frames / nominal
	return fmt.Sprintf("%02d:%02d:%02d:%02d", s/3600, s/60%60, s%60, f)
}
