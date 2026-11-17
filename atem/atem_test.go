package atem

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"resolve-sync/timebase"
)

func TestParseSmall(t *testing.T) {
	rec, err := ParseFile("testdata/small.drp")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Rate != timebase.Int(25) || rec.Duration != 20*25 {
		t.Errorf("rate %v duration %d", rec.Rate, rec.Duration)
	}
	// Crosses midnight; the mix is centred on 23:59:59:00 = frame 225.
	want := []Segment{{1, 0, 100}, {2, 100, 225}, {1, 225, 375}, {8, 375, 500}}
	if !reflect.DeepEqual(rec.Program, want) {
		t.Errorf("program = %v\nwant %v", rec.Program, want)
	}
	wantTr := []Transition{{Type: "Mix", From: 2, To: 1, Start: 200, End: 250}}
	if !reflect.DeepEqual(rec.Transitions, wantTr) {
		t.Errorf("transitions = %v (the aborted one must not appear)", rec.Transitions)
	}
	if rec.Ignored["downstream key changes"] != 1 {
		t.Errorf("ignored = %v", rec.Ignored)
	}
	if cams := rec.Cameras(); len(cams) != 2 || cams[1].Name != "right" {
		t.Errorf("cameras = %v", cams)
	}
	// The switcher rolled over to a second file 14.4 s in.
	wantFiles := []SourceFile{{"Video ISO Files/X CAM 2 01.mp4", 0}, {"Video ISO Files/X CAM 2 02.mp4", 360}}
	if got := rec.Cameras()[1].Files; !reflect.DeepEqual(got, wantFiles) {
		t.Errorf("files = %v", got)
	}
	if s, _ := rec.Source(8); s.IsCamera() {
		t.Error("the still store is not a camera")
	}
}

func TestParseSample(t *testing.T) {
	const path = "../SOKFehtarji.drp"
	if _, err := os.Stat(path); err != nil {
		t.Skip("sample project not present")
	}
	rec, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Rate != timebase.Int(30) || len(rec.Cameras()) != 4 {
		t.Errorf("rate %v, %d cameras", rec.Rate, len(rec.Cameras()))
	}
	if len(rec.Transitions) != 89 {
		t.Errorf("%d transitions, want 89", len(rec.Transitions))
	}
	// 626 source changes plus the opening segment.
	if len(rec.Program) != 627 {
		t.Errorf("%d segments, want 627", len(rec.Program))
	}
	if got := timebase.FormatTimecode(rec.Duration, 30); got != "01:53:51:26" {
		t.Errorf("duration = %s", got)
	}
	// Segments tile the recording with no holes and no repeats.
	var at int64
	for i, s := range rec.Program {
		if s.Start != at || s.End <= s.Start {
			t.Fatalf("segment %d = %+v, expected start %d", i, s, at)
		}
		if i > 0 && rec.Program[i-1].Source == s.Source {
			t.Fatalf("segment %d repeats source %d", i, s.Source)
		}
		at = s.End
	}
	if at != rec.Duration {
		t.Errorf("program ends at %d, recording at %d", at, rec.Duration)
	}
}

func TestParseRejectsOtherFiles(t *testing.T) {
	if _, err := Parse(strings.NewReader("PK\x03\x04 binary")); err == nil || !strings.Contains(err.Error(), "not an ATEM project file") {
		t.Errorf("err = %v", err)
	}
	if _, err := Parse(strings.NewReader(`{"hello":1}`)); err == nil {
		t.Error("want error")
	}
}

func TestParseVideoMode(t *testing.T) {
	for mode, want := range map[string]timebase.Rat{
		"1080p30": timebase.Int(30), "1080p29.97": timebase.New(30000, 1001),
		"1080i50": timebase.Int(25), "2160p59.94": timebase.New(60000, 1001), "720p50": timebase.Int(50),
	} {
		if got, err := ParseVideoMode(mode); err != nil || got != want {
			t.Errorf("%s = %v, %v", mode, got, err)
		}
	}
}
