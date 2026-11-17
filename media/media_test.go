package media

import (
	"testing"

	"resolve-sync/timebase"
)

func TestParseProbe(t *testing.T) {
	const out = `{"streams":[
	 {"codec_type":"video","width":1920,"height":1080,"r_frame_rate":"30000/1001","avg_frame_rate":"30000/1001","nb_frames":"300","duration":"10.010000","disposition":{"attached_pic":0}},
	 {"codec_type":"audio","channels":2,"sample_rate":"48000"},
	 {"codec_type":"data","tags":{"timecode":"01:00:00:00"}}],
	 "format":{"duration":"10.010000","tags":{"creation_time":"2026-05-01T18:49:11.000000Z"}}}`
	info, err := parseProbe([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if info.Rate != timebase.New(30000, 1001) || info.Frames != 300 || info.Duration != timebase.New(1001, 100) {
		t.Errorf("%+v", info)
	}
	// 01:00:00:00 non-drop at 29.97 is 108000 frames = 3603.6 s.
	if info.TCStart != timebase.New(36036, 10) {
		t.Errorf("TCStart = %v", info.TCStart)
	}
	if !info.HasAudio || info.AudioRate != 48000 || info.Created.IsZero() {
		t.Errorf("%+v", info)
	}
}

func TestParseProbeNoFrameCount(t *testing.T) {
	const out = `{"streams":[{"codec_type":"video","width":3840,"height":2160,"r_frame_rate":"25/1","duration":"12.48"}],"format":{"duration":"12.5"}}`
	info, err := parseProbe([]byte(out))
	if err != nil || info.Frames != 312 || info.HasAudio {
		t.Errorf("%+v, %v", info, err)
	}
	if _, err := parseProbe([]byte(`{"streams":[{"codec_type":"data"}],"format":{}}`)); err == nil {
		t.Error("want error")
	}
}
