package fcpxml

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"resolve-sync/media"
	"resolve-sync/timebase"
)

func clipMedia(name string, rate timebase.Rat, tc timebase.Rat) media.Info {
	return media.Info{Path: "/footage/cam " + name + "/" + name + " & co.mov", Name: name, Rate: rate, Frames: 500,
		Duration: timebase.FrameTime(500, rate), Width: 1920, Height: 1080, TCStart: tc,
		HasVideo: true, HasAudio: true, AudioChannels: 2, AudioRate: 48000}
}

// spikeProject reproduces spike/2-full.fcpxml: camB starts 2 s late and has a
// hole from 12 s to 15 s, audio stays on camA, one dissolve at 10 s.
func spikeProject() *Project {
	a := clipMedia("camA", timebase.Int(25), timebase.Rat{})
	b := clipMedia("camB", timebase.Int(25), timebase.Rat{})
	return &Project{
		Name: "Spike2", Rate: timebase.Int(25), Width: 1920, Height: 1080, Frames: 500,
		Angles: []Angle{
			{Name: "camA", Clips: []Clip{{Media: a, Offset: 0, Frames: 500}}},
			{Name: "camB", Clips: []Clip{
				{Media: b, Offset: 50, Frames: 250},
				{Media: b, Offset: 375, Frames: 125, SrcStart: timebase.Int(13)},
			}},
		},
		Cuts:       []Cut{{0, 0, 125}, {1, 125, 250}, {0, 250, 325}, {1, 325, 500}},
		Dissolves:  []Dissolve{{At: 250, Frames: 25}},
		AudioAngle: 0,
	}
}

func TestWriteMatchesSpikeStructure(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, spikeProject()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`<format id="r1" name="FFVideoFormat1080p25" frameDuration="1/25s" width="1920" height="1080"/>`,
		`src="file:///footage/cam%20camA/camA%20&amp;%20co.mov"`,
		`<gap name="Gap" offset="0s" start="0s" duration="2s"/>`,
		`<asset-clip ref="r3" name="camB" offset="2s" start="0s" duration="10s" format="r1" tcFormat="NDF"/>`,
		`<gap name="Gap" offset="12s" start="0s" duration="3s"/>`,
		`<asset-clip ref="r3" name="camB" offset="15s" start="13s" duration="5s" format="r1" tcFormat="NDF"/>`,
		`<transition name="Cross Dissolve" offset="238/25s" duration="1s"/>`,
		`<mc-clip ref="r4" name="Spike2 Multicam" offset="13s" start="13s" duration="7s">`,
		`<mc-source angleID="angle2" srcEnable="video"/>`,
		`<mc-source angleID="angle1" srcEnable="audio"/>`,
		`<mc-source angleID="angle1" srcEnable="all"/>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %s", want)
		}
	}
	// Well-formed XML.
	dec := xml.NewDecoder(strings.NewReader(out))
	for {
		if _, err := dec.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("not well-formed: %v\n%s", err, out)
		}
	}
}

func TestWriteMixedRatesAndTimecode(t *testing.T) {
	p := spikeProject()
	ntsc := timebase.New(30000, 1001)
	p.Angles[1].Clips = []Clip{{Media: clipMedia("camB", ntsc, timebase.FrameTime(108000, ntsc)), Offset: 0, Frames: 250, SrcStart: timebase.New(1001, 1000)}}
	var buf bytes.Buffer
	if err := Write(&buf, p); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`<format id="r3" name="FFVideoFormat1080p2997" frameDuration="1001/30000s"`,
		// Source position = embedded timecode (3603.6 s) + 1.001 s into the file.
		`start="3604601/1000s" duration="10s" format="r3"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %s\n%s", want, out)
		}
	}
}

func TestValidate(t *testing.T) {
	bad := []func(*Project){
		func(p *Project) { p.Cuts[1].Start = 130 },             // hole between cuts
		func(p *Project) { p.Cuts = p.Cuts[:3] },               // stops early
		func(p *Project) { p.Dissolves[0].At = 251 },           // not on a cut
		func(p *Project) { p.Angles[1].Clips[1].Offset = 290 }, // overlaps previous clip
		func(p *Project) { p.AudioAngle = 2 },                  // no such angle
		func(p *Project) { p.Cuts[0].Angle = 5 },               // no such angle
		func(p *Project) { p.Angles[0].Clips[0].Frames = 501 }, // past the end
	}
	for i, mutate := range bad {
		p := spikeProject()
		mutate(p)
		if err := Write(io.Discard, p); err == nil {
			t.Errorf("case %d: want error", i)
		}
	}
}
