// Command audioprobe is a development tool: it lines up two clips by sound
// and prints the evidence. It only reads the footage.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"resolve-sync/align"
)

func main() {
	aPath := flag.String("a", "", "reference clip")
	bPath := flag.String("b", "", "clip to place")
	aLen := flag.Float64("alen", 1800, "seconds of the reference to search, from its start")
	bFrom := flag.Float64("bfrom", 30, "where in the clip to take the sample")
	bLen := flag.Float64("blen", 240, "seconds of the clip to sample")
	flag.Parse()
	ctx := context.Background()
	var dec align.Decoder
	const rate, hop = 8000, 160 // envelope at 50 values a second

	t0 := time.Now()
	a, err := dec.Audio(ctx, *aPath, 0, *aLen, rate)
	check(err)
	b, err := dec.Audio(ctx, *bPath, *bFrom, *bLen, rate)
	check(err)
	t1 := time.Now()
	p, ok := align.Correlate(align.Envelope(a, hop, 100), align.Envelope(b, hop, 100), false, 25)
	coarse := p.Offset*hop/rate - *bFrom // seconds: reference time = clip time + coarse
	fmt.Printf("decode %.1fs  coarse: ok=%v b starts %.2fs into a  score=%.3f runner-up=%.3f  (%.2fs)\n",
		t1.Sub(t0).Seconds(), ok, coarse, p.Score, p.RunnerUp, time.Since(t1).Seconds())
	if !ok {
		return
	}
	// Refine on a much finer loudness contour, one value per millisecond.
	const fineHop = 8
	bMid := *bFrom + *bLen/2 - 30
	fb, err := dec.Audio(ctx, *bPath, bMid, 60, rate)
	check(err)
	aStart := bMid + coarse - 2
	if aStart < 0 {
		aStart = 0
	}
	fa, err := dec.Audio(ctx, *aPath, aStart, 64, rate)
	check(err)
	f, ok := align.Correlate(align.Envelope(fa, fineHop, 250), align.Envelope(fb, fineHop, 250), false, 50)
	fine := aStart + f.Offset*fineHop/rate - bMid
	fmt.Printf("fine:   ok=%v b starts %.4fs into a  score=%.3f runner-up=%.3f\n", ok, fine, f.Score, f.RunnerUp)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
