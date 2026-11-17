package timebase

import "testing"

func TestRat(t *testing.T) {
	if got := New(19, 2).String(); got != "19/2s" {
		t.Errorf("String = %s", got)
	}
	if got := New(10, 2).String(); got != "5s" {
		t.Errorf("String = %s", got)
	}
	ntsc := New(30000, 1001)
	if got := FrameTime(30000, ntsc); got != Int(1001) {
		t.Errorf("FrameTime = %v", got)
	}
	// One hour of 30p frames lands on the 25p grid exactly.
	if got := TimeFrame(FrameTime(108000, Int(30)), Int(25)); got != 90000 {
		t.Errorf("TimeFrame = %d", got)
	}
	if New(-1, 2).Round() != -1 || New(1, 2).Round() != 1 || New(49, 100).Round() != 0 {
		t.Error("Round")
	}
	if New(-1, 2).Floor() != -1 || New(-1, 2).Ceil() != 0 || New(3, 2).Ceil() != 2 {
		t.Error("Floor/Ceil")
	}
	if Int(1).Div(New(1, 3)) != Int(3) || New(1, 3).Cmp(New(1, 2)) != -1 {
		t.Error("Div/Cmp")
	}
}

func TestParseRate(t *testing.T) {
	for in, want := range map[string]Rat{
		"30000/1001": New(30000, 1001), "25/1": Int(25), "30": Int(30),
		"29.97": New(30000, 1001), "59.94": New(60000, 1001), "23.976": New(24000, 1001),
	} {
		got, err := ParseRate(in)
		if err != nil || got != want {
			t.Errorf("ParseRate(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseRate("0/0"); err == nil {
		t.Error("want error")
	}
}

func TestTimecode(t *testing.T) {
	got, err := ParseTimecode("18:49:11:03", 30)
	if err != nil || got != ((18*60+49)*60+11)*30+3 {
		t.Errorf("got %d, %v", got, err)
	}
	// 00:01:00;02 is the frame right after 00:00:59;29 in drop-frame.
	a, _ := ParseTimecode("00:00:59;29", 30)
	b, _ := ParseTimecode("00:01:00;02", 30)
	if b-a != 1 {
		t.Errorf("drop-frame step = %d", b-a)
	}
	// One hour of drop-frame timecode is 107892 real frames.
	if h, _ := ParseTimecode("01:00:00;00", 30); h != 107892 {
		t.Errorf("drop-frame hour = %d", h)
	}
	if _, err := ParseTimecode("00:00:00:30", 30); err == nil {
		t.Error("want error for frame 30 at 30 fps")
	}
	if s := FormatTimecode(got, 30); s != "18:49:11:03" {
		t.Errorf("format = %s", s)
	}
}
