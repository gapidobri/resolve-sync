// Command syncprobe is a development tool: it matches one raw clip against
// one ISO file and prints the evidence. It only reads the footage.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"resolve-sync/align"
	"resolve-sync/media"
)

func main() {
	isoPath := flag.String("iso", "", "ISO file")
	rawPath := flag.String("raw", "", "raw clip")
	trackLen := flag.Float64("track", 900, "seconds of ISO to search, from its start")
	trackFrom := flag.Float64("from", 0, "where in the ISO file the search starts")
	span := flag.Float64("span", 300, "seconds of the raw clip to sample, from its start")
	nSamples := flag.Int("n", 24, "raw frames to sample")
	fineAt := flag.String("fine", "", "skip the coarse search: comma-separated raw clip times to fine-match at")
	guess := flag.Float64("guess", 0, "offset to refine, with -fine")
	flag.Parse()
	ctx := context.Background()
	var dec align.Decoder
	iso, err := media.Prober{}.Probe(ctx, *isoPath)
	check(err)
	raw, err := media.Prober{}.Probe(ctx, *rawPath)
	check(err)
	fmt.Printf("ISO %s  %.2f fps  %.0f s\nRAW %s  %.2f fps  %.0f s\n", iso.Name, iso.Rate.Float(), iso.Duration.Float(), raw.Name, raw.Rate.Float(), raw.Duration.Float())

	if *fineAt != "" {
		for _, s := range strings.Split(*fineAt, ",") {
			rt, err := strconv.ParseFloat(s, 64)
			check(err)
			fine(ctx, dec, raw, iso, rt, *guess)
		}
		return
	}
	const cw, ch = 32, 18
	t0 := time.Now()
	var track []align.Frame
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		track, err = dec.Track(ctx, iso.Path, *trackFrom, *trackLen, 1, cw, ch)
		check(err)
	}()
	rawRate := raw.Rate.Float()
	*span = math.Min(*span, raw.Duration.Float()-2)
	samples := make([]align.Frame, *nSamples)
	times := make([]float64, *nSamples)
	sem := make(chan struct{}, 6)
	for k := range samples {
		frame := int64((1 + (*span-2)*float64(k)/float64(*nSamples-1)) * rawRate)
		times[k] = float64(frame) / rawRate
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			f, err := dec.Window(ctx, raw.Path, raw.Rate, frame, 1, cw, ch)
			check(err)
			if len(f) == 1 {
				samples[k] = f[0]
			}
		}()
	}
	wg.Wait()
	fmt.Printf("decoded %d ISO track frames and %d raw samples in %.1fs\n", len(track), len(samples), time.Since(t0).Seconds())

	p, ok := align.Coarse(samples, times, track, *trackFrom, 1)
	fmt.Printf("coarse: ok=%v offset=%.0fs score=%.3f runner-up=%.3f margin=%.1f\n", ok, p.Offset, p.Score, p.RunnerUp, p.Margin)
	if !ok {
		return
	}

	for _, rt := range []float64{times[2], times[len(times)/2], times[len(times)-3]} {
		fine(ctx, dec, raw, iso, rt, p.Offset)
	}
}

func fine(ctx context.Context, dec align.Decoder, raw, iso media.Info, rt, guess float64) {
	const fw, fh = 64, 36
	rawRate, isoRate := raw.Rate.Float(), iso.Rate.Float()
	t1 := time.Now()
	first := int64(rt * rawRate)
	rw, err := dec.Window(ctx, raw.Path, raw.Rate, first, int(4*rawRate), fw, fh)
	check(err)
	isoFirst := int64((float64(first)/rawRate + guess - 2.5) * isoRate)
	if isoFirst < 0 {
		isoFirst = 0
	}
	iw, err := dec.Window(ctx, iso.Path, iso.Rate, isoFirst, int(9*isoRate), fw, fh)
	check(err)
	f, ok := align.Fine(rw, float64(first)/rawRate, rawRate, iw, float64(isoFirst)/isoRate, isoRate, guess, 2)
	fmt.Printf("fine at raw %.0fs: ok=%v offset=%.4fs score=%.3f runner-up=%.3f  (%.1fs)\n", rt, ok, f.Offset, f.Score, f.RunnerUp, time.Since(t1).Seconds())
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
