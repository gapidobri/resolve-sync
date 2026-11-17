package assemble

import (
	"io"
	"os"
	"strings"
	"testing"

	"resolve-sync/atem"
	"resolve-sync/fcpxml"
	"resolve-sync/media"
	"resolve-sync/timebase"
)

func vid(name string, rate timebase.Rat, seconds int64) media.Info {
	return media.Info{Path: "/f/" + name + ".mov", Name: name, Rate: rate, Duration: timebase.Int(seconds),
		Frames: timebase.Int(seconds).Mul(rate).Round(), Width: 3840, Height: 2160, HasVideo: true, HasAudio: true, AudioChannels: 2, AudioRate: 48000}
}

// A 30p switcher recording of 20 s: cam 1 until 4 s, cam 2 until a mix
// centred on 9 s, cam 1 until 15 s, then a still.
func recording() *atem.Recording {
	return &atem.Recording{
		VideoMode: "1080p30", Rate: timebase.Int(30), Duration: 600,
		Sources:     []atem.Source{{Index: 1, Name: "left", Type: "Video", Files: []atem.SourceFile{{File: "a.mp4"}}}, {Index: 2, Name: "right", Type: "Video", Files: []atem.SourceFile{{File: "b.mp4"}}}, {Index: 8, Name: "Media Player 1", Type: "Still"}},
		Program:     []atem.Segment{{Source: 1, Start: 0, End: 120}, {Source: 2, Start: 120, End: 270}, {Source: 1, Start: 270, End: 450}, {Source: 8, Start: 450, End: 600}},
		Transitions: []atem.Transition{{Type: "Mix", From: 2, To: 1, Start: 240, End: 300}},
		Ignored:     map[string]int{"downstream key changes": 3},
	}
}

func TestBuild(t *testing.T) {
	r25 := timebase.Int(25)
	angles := []Angle{
		// Started 90.5 s before the switcher and runs past its end.
		{Name: "Left", Source: 1, Clips: []PlacedClip{{Media: vid("L1", r25, 300), Offset: timebase.New(-181, 2)}}},
		// Two clips with a hole from 5 s to 8 s, plus one from before the show.
		{Name: "Right", Source: 2, Clips: []PlacedClip{
			{Media: vid("R2", r25, 12), Offset: timebase.Int(8)},
			{Media: vid("R1", r25, 5), Offset: timebase.Int(0)},
			{Media: vid("R0", r25, 60), Offset: timebase.Int(-600)},
		}},
		{Name: "Roaming", Source: NoSource, Clips: []PlacedClip{{Media: vid("X", timebase.Int(50), 10), Offset: timebase.Int(2)}}},
	}
	p, rep, err := Build(recording(), angles, Options{Name: "Show", AudioAngle: 0})
	if err != nil {
		t.Fatal(err)
	}
	if p.Rate != r25 || p.Frames != 500 || p.Width != 3840 {
		t.Errorf("rate %v frames %d width %d", p.Rate, p.Frames, p.Width)
	}
	left := p.Angles[0].Clips
	if len(left) != 1 || left[0].Offset != 0 || left[0].Frames != 500 || left[0].SrcStart != timebase.New(2263, 25) {
		t.Errorf("left = %+v", left)
	}
	right := p.Angles[1].Clips
	if len(right) != 2 || right[0].Media.Name != "R1" || right[0].Frames != 125 || right[1].Offset != 200 || right[1].Frames != 300 {
		t.Errorf("right = %+v", right)
	}
	if len(rep.Unused) != 1 || !strings.HasSuffix(rep.Unused[0], "R0.mov") {
		t.Errorf("unused = %v", rep.Unused)
	}
	want := []fcpxml.Cut{{Angle: 0, Start: 0, End: 100}, {Angle: 1, Start: 100, End: 225}, {Angle: 0, Start: 225, End: 375}, {Angle: -1, Start: 375, End: 500}}
	if len(p.Cuts) != len(want) {
		t.Fatalf("cuts = %+v", p.Cuts)
	}
	for i := range want {
		if p.Cuts[i] != want[i] {
			t.Errorf("cut %d = %+v, want %+v", i, p.Cuts[i], want[i])
		}
	}
	// The 2 s mix at 30p is 50 frames at 25p, with footage on both sides.
	if len(p.Dissolves) != 1 || p.Dissolves[0] != (fcpxml.Dissolve{At: 225, Frames: 50}) {
		t.Errorf("dissolves = %+v", p.Dissolves)
	}
	// Right was on air 4 s-9 s but has no footage 5 s-8 s.
	if len(rep.OnAirGaps) != 1 || rep.OnAirGaps[0] != (Span{"Right", 125, 200}) {
		t.Errorf("gaps = %+v", rep.OnAirGaps)
	}
	notes := strings.Join(rep.Notes, "\n")
	for _, s := range []string{`"Media Player 1" (Still)`, "downstream key changes (3)"} {
		if !strings.Contains(notes, s) {
			t.Errorf("notes lack %q:\n%s", s, notes)
		}
	}
	if err := fcpxml.Write(io.Discard, p); err != nil {
		t.Errorf("result does not validate: %v", err)
	}
}

func TestDissolveIntoGapBecomesCut(t *testing.T) {
	r25 := timebase.Int(25)
	angles := []Angle{
		{Name: "Left", Source: 1, Clips: []PlacedClip{{Media: vid("L1", r25, 20), Offset: timebase.Int(0)}}},
		// Ends at 8.5 s, half a second before the mix is centred.
		{Name: "Right", Source: 2, Clips: []PlacedClip{{Media: vid("R1", r25, 20), Offset: timebase.New(-23, 2)}}},
	}
	p, rep, err := Build(recording(), angles, Options{AudioAngle: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Dissolves) != 0 || !strings.Contains(strings.Join(rep.Notes, "\n"), "1 dissolves became hard cuts because one of the two cameras has no footage") {
		t.Errorf("dissolves = %+v notes = %v", p.Dissolves, rep.Notes)
	}
}

func TestBuildErrors(t *testing.T) {
	a := Angle{Name: "A", Source: 1, Clips: []PlacedClip{{Media: vid("a", timebase.Int(25), 20)}}}
	b := Angle{Name: "B", Source: 1, Clips: []PlacedClip{{Media: vid("b", timebase.Int(25), 20)}}}
	if _, _, err := Build(recording(), []Angle{a, b}, Options{AudioAngle: -1}); err == nil {
		t.Error("two angles on one input must fail")
	}
	if _, _, err := Build(recording(), []Angle{a}, Options{AudioAngle: 3}); err == nil {
		t.Error("bad audio angle must fail")
	}
	if _, _, err := Build(recording(), nil, Options{}); err == nil {
		t.Error("no angles must fail")
	}
}

// The real show, with stand-in footage covering every camera for its whole
// length: every cut and every mix must survive.
func TestBuildSample(t *testing.T) {
	const path = "../SOKFehtarji.drp"
	if _, err := os.Stat(path); err != nil {
		t.Skip("sample project not present")
	}
	rec, err := atem.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var angles []Angle
	for _, cam := range rec.Cameras() {
		angles = append(angles, Angle{Name: cam.Name, Source: cam.Index,
			Clips: []PlacedClip{{Media: vid(cam.Name, timebase.Int(30), 7200), Offset: timebase.Int(-60)}}})
	}
	p, rep, err := Build(rec, angles, Options{Name: "SOKFehtarji", AudioAngle: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cuts != 626 || rep.Dissolves != 89 || len(rep.OnAirGaps) != 0 || len(rep.Notes) != 0 {
		t.Errorf("cuts %d dissolves %d gaps %d notes %v", rep.Cuts, rep.Dissolves, len(rep.OnAirGaps), rep.Notes)
	}
	if err := fcpxml.Write(io.Discard, p); err != nil {
		t.Error(err)
	}
}

func TestMinShot(t *testing.T) {
	rec := recording()
	// cam 1, a 3-frame slip to cam 2, cam 1 again, 8 frames of cam 2, then the still.
	rec.Program = []atem.Segment{{Source: 1, Start: 0, End: 120}, {Source: 2, Start: 120, End: 123}, {Source: 1, Start: 123, End: 300},
		{Source: 2, Start: 300, End: 310}, {Source: 8, Start: 310, End: 600}}
	rec.Transitions = nil
	r25 := timebase.Int(25)
	angles := []Angle{
		{Name: "Left", Source: 1, Clips: []PlacedClip{{Media: vid("L", r25, 20)}}},
		{Name: "Right", Source: 2, Clips: []PlacedClip{{Media: vid("R", r25, 20)}}},
	}
	p, rep, err := Build(rec, angles, Options{AudioAngle: -1, MinShotFrames: 12})
	if err != nil {
		t.Fatal(err)
	}
	// Both short shots go: the slip rejoins cam 1, and cam 1 carries on over the 8 frames.
	want := []fcpxml.Cut{{Angle: 0, Start: 0, End: 258}, {Angle: -1, Start: 258, End: 500}}
	if len(p.Cuts) != 2 || p.Cuts[0] != want[0] || p.Cuts[1] != want[1] {
		t.Errorf("cuts = %+v", p.Cuts)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "2 shots shorter than 12 frames were removed") {
		t.Errorf("notes = %v", rep.Notes)
	}
	if err := fcpxml.Write(io.Discard, p); err != nil {
		t.Error(err)
	}
	// A short shot at the very start is taken over by the one after it.
	rec.Program = []atem.Segment{{Source: 2, Start: 0, End: 6}, {Source: 1, Start: 6, End: 600}}
	p, _, err = Build(rec, angles, Options{AudioAngle: -1, MinShotFrames: 12})
	if err != nil || len(p.Cuts) != 1 || p.Cuts[0] != (fcpxml.Cut{Angle: 0, Start: 0, End: 500}) {
		t.Errorf("cuts = %+v, %v", p.Cuts, err)
	}
}

func TestPartsOfOneFile(t *testing.T) {
	r25 := timebase.Int(25)
	m := vid("L", r25, 20)
	// The second half sits one frame earlier, and the halves overlap by 3 frames.
	angles := []Angle{{Name: "Left", Source: 1, Clips: []PlacedClip{
		{Media: m, Offset: timebase.Int(0), Out: timebase.New(253, 25)},
		{Media: m, Offset: timebase.New(-1, 25), In: timebase.Int(10)},
	}}}
	p, rep, err := Build(recording(), angles, Options{AudioAngle: -1})
	if err != nil {
		t.Fatal(err)
	}
	c := p.Angles[0].Clips
	if len(c) != 2 || c[0].Frames != 253 || c[1].Offset != 253 || c[1].SrcStart != timebase.New(254, 25) || c[1].Offset+c[1].Frames != 499 {
		t.Errorf("clips = %+v", c)
	}
	if strings.Contains(strings.Join(rep.Notes, "\n"), "overlaps") {
		t.Errorf("parts of one file must not be reported as overlapping: %v", rep.Notes)
	}
}
