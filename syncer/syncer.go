package syncer

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"resolve-sync/align"
	"resolve-sync/atem"
	"resolve-sync/media"
)

// Folder is one camera's footage as selected by the user.
type Folder struct {
	Name  string
	Clips []media.Info
	// Input fixes which switcher input the camera fed: a source index, or
	// NotConnected. Zero lets the picture decide.
	Input int
}

// NotConnected marks a camera that was not plugged into the switcher.
const NotConnected = -1

// How a clip's position was established, from most to least exact.
const (
	ByPicture = "picture" // matched frame by frame against the ISO recording
	BySound   = "sound"   // lined up with another camera's audio
	ByClock   = "clock"   // inferred from the camera's own clock
)

// Clip is the outcome for one file.
type Clip struct {
	Media  media.Info
	Placed bool
	// Offset is the time in seconds from the start of the switcher
	// recording to the clip's first frame; negative if the camera started
	// first.
	Offset float64
	Method string
	// Drift is how much the offset changed between the first and the last
	// picture match in the clip, in seconds. Camera and switcher clocks run
	// at slightly different speeds.
	Drift float64
	// Windows is how many independent picture matches agreed on the offset.
	Windows int
	// Parts is set when the drift is about a frame or more: the clip is then
	// placed as consecutive parts, each with its own offset, so that no part
	// is more than about a quarter of a frame out. One frame is repeated or
	// skipped at each join.
	Parts []Part
	// Warning is set when the placement should be checked by eye.
	Warning string
	// Wave is the clip's sound level, one value a second.
	Wave align.Waveform
}

// Part is a stretch of a clip with its own offset.
type Part struct {
	In, Out float64 // seconds into the clip; the parts overlap slightly
	Offset  float64 // as Clip.Offset, for this part
}

// Angle is the outcome for one folder.
type Angle struct {
	Name string
	// Source is the switcher input the camera fed, or -1 if its picture
	// matches none of them.
	Source int
	Clips  []Clip
}

// Result is the outcome of a run.
type Result struct {
	Angles []Angle
	// Master is the separately recorded mix, if one was given.
	Master *Clip
	Notes  []string
}

// Options configure a run.
type Options struct {
	Decoder align.Decoder
	// Workers is how many ffmpeg processes may run at once; 0 means 6.
	Workers int
	// MasterAudio is an optional recording of the PA mix to line up with
	// the cameras.
	MasterAudio *media.Info
	// Progress, if set, receives a report whenever a stage advances or a
	// clip changes state. It may be called from several goroutines at once.
	Progress func(Progress)
}

// Stages of a run, in order.
const (
	StageSound  = "sound"  // reading every clip's audio
	StageInputs = "inputs" // finding each camera in the switcher recordings
	StageMatch  = "match"  // frame-by-frame placement of every clip
	StageMix    = "mix"    // lining up the PA mix
)

// Clip states reported while a run is in progress.
const (
	ClipListening = "listening"
	ClipHeard     = "heard" // sound read; Progress.Wave carries its waveform
	ClipSearching = "searching"
	ClipMatching  = "matching"
	ClipPlaced    = "placed"
	ClipFailed    = "failed"
)

// Progress is one report from a run.
type Progress struct {
	Stage string `json:"stage"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
	// Text describes what is happening right now, for display.
	Text string `json:"text"`
	// Folder and Clip identify the clip a state change is about (indices
	// into the folders and their clips); Folder is -1 for the PA mix, and
	// both are -2 when the report is not about a clip.
	Folder int    `json:"folder"`
	Clip   int    `json:"clip"`
	State  string `json:"state"`
	// Offset and Method are set with ClipPlaced.
	Offset float64 `json:"offset"`
	Method string  `json:"method"`
	// Wave is set with ClipHeard.
	Wave align.Waveform `json:"wave,omitempty"`
}

// Thresholds, set from measurements on real multi-camera concert footage.
const (
	// A sound match is trusted when its peak is this many times the best
	// competing peak. Real matches measured 2.5x and up, false ones 1.1x.
	soundRatio = 1.8
	// A coarse picture match is trusted above this similarity and this lead
	// over the best different answer. Real: 0.5-0.9 with lead 0.09-0.3;
	// wrong camera: 0.2 with lead 0.01.
	coarseScore, coarseLead = 0.3, 0.05
	// Same for a frame-by-frame window. Real: 0.35-0.96; wrong camera: 0.04.
	fineScore, fineLead = 0.25, 0.04

	audioRate = 8000
	audioHop  = 40  // one loudness value per 5 ms
	audioWin  = 400 // trend removed over 2 s either side
	windowSec = 4.0
)

type clipState struct {
	folder, index int
	info          media.Info
	env           []float32
	noPicture     bool // its picture cannot be decoded on this machine
	wave          align.Waveform
	group         int     // clips in one group are placed relative to each other
	pos           float64 // start time within the group, seconds
	method        string  // how pos was found
	result        Clip
}

type engine struct {
	ctx     context.Context
	opt     Options
	rec     *atem.Recording
	iso     *ISOSet
	sem     chan struct{}
	notes   []string
	mu      sync.Mutex
	anchors map[int]float64     // group -> seconds to add to pos to get switcher time
	found   map[*clipState]bool // clips located by the picture search itself

	// Progress of the current stage, guarded by mu.
	stage, text string
	done, total int
	tracks      map[string][]align.Frame
}

// begin starts a stage with the given number of steps.
func (e *engine) begin(stage string, total int, text string) {
	e.mu.Lock()
	e.stage, e.done, e.total, e.text = stage, 0, total, text
	e.mu.Unlock()
	e.report(Progress{Folder: -2, Clip: -2})
}

// advance counts one step of the current stage as done.
func (e *engine) advance() {
	e.mu.Lock()
	if e.done < e.total {
		e.done++
	}
	e.mu.Unlock()
	e.report(Progress{Folder: -2, Clip: -2})
}

// say replaces the description of what is happening now.
func (e *engine) say(format string, a ...any) {
	e.mu.Lock()
	e.text = fmt.Sprintf(format, a...)
	e.mu.Unlock()
	e.report(Progress{Folder: -2, Clip: -2})
}

// clipState reports a clip's new state.
func (e *engine) clipState(c *clipState, state string) {
	p := Progress{Folder: c.folder, Clip: c.index, State: state}
	if state == ClipHeard {
		p.Wave = c.wave
	}
	if state == ClipPlaced {
		p.Offset, p.Method = c.result.Offset, c.result.Method
	}
	e.report(p)
}

func (e *engine) report(p Progress) {
	if e.opt.Progress == nil {
		return
	}
	e.mu.Lock()
	p.Stage, p.Done, p.Total, p.Text = e.stage, e.done, e.total, e.text
	e.mu.Unlock()
	e.opt.Progress(p)
}

func (e *engine) notef(format string, a ...any) {
	e.mu.Lock()
	e.notes = append(e.notes, fmt.Sprintf(format, a...))
	e.mu.Unlock()
}

// limit runs f while holding one of the worker slots.
func (e *engine) limit(f func()) {
	e.sem <- struct{}{}
	defer func() { <-e.sem }()
	f()
}

// Run places every clip.
func Run(ctx context.Context, rec *atem.Recording, iso *ISOSet, folders []Folder, opt Options) (*Result, error) {
	if opt.Workers <= 0 {
		opt.Workers = 6
	}
	e := &engine{ctx: ctx, opt: opt, rec: rec, iso: iso, sem: make(chan struct{}, opt.Workers),
		anchors: map[int]float64{}, found: map[*clipState]bool{}, tracks: map[string][]align.Frame{}}

	var clips []*clipState
	for fi, f := range folders {
		for ci, info := range f.Clips {
			clips = append(clips, &clipState{folder: fi, index: ci, info: info, group: len(clips)})
		}
	}
	if len(clips) == 0 {
		return nil, fmt.Errorf("no footage selected")
	}
	var master *clipState
	if opt.MasterAudio != nil {
		master = &clipState{folder: -1, info: *opt.MasterAudio, group: len(clips)}
	}

	// Stage 1: sound.
	withMaster := clips
	if master != nil {
		withMaster = append(append([]*clipState(nil), clips...), master)
	}
	if err := e.loadSound(withMaster); err != nil {
		return nil, err
	}
	e.groupBySound(clips)
	e.groupByClock(clips, folders)
	e.checkPicture(clips)

	// Stages 2 and 3: picture.
	assigned := e.assignInputs(clips, folders)
	e.begin(StageMatch, len(clips), "Matching")
	var wg sync.WaitGroup
	for _, c := range clips {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.finish(c, assigned[c.folder])
			if c.result.Placed {
				e.clipState(c, ClipPlaced)
			} else {
				e.clipState(c, ClipFailed)
			}
			e.advance()
		}()
	}
	wg.Wait()

	res := &Result{}
	for fi, f := range folders {
		a := Angle{Name: f.Name, Source: assigned[fi]}
		for _, c := range clips {
			if c.folder == fi {
				a.Clips = append(a.Clips, c.result)
			}
		}
		if a.Source < 0 && f.Input == 0 && !e.allUndecodable(clips, fi) {
			e.notef("%s matches no switcher input; placed by sound only.", f.Name)
		}
		res.Angles = append(res.Angles, a)
	}
	if master != nil {
		e.begin(StageMix, 1, "Lining up the PA mix")
		res.Master = e.placeMaster(master, clips)
		master.result = *res.Master
		if res.Master.Placed {
			e.clipState(master, ClipPlaced)
		} else {
			e.clipState(master, ClipFailed)
		}
		e.advance()
	}
	sort.Strings(e.notes)
	res.Notes = e.notes
	return res, nil
}

func (e *engine) allUndecodable(clips []*clipState, folder int) bool {
	for _, c := range clips {
		if c.folder == folder && !c.noPicture {
			return false
		}
	}
	return true
}

// checkPicture finds clips whose picture cannot be decoded, by trying one
// frame of one clip per file type, so that the picture stages do not spend
// minutes failing on them.
func (e *engine) checkPicture(clips []*clipState) {
	decodable := map[string]bool{}
	for _, c := range clips {
		if !c.info.HasVideo {
			continue
		}
		ext := strings.ToLower(filepath.Ext(c.info.Path))
		ok, seen := decodable[ext]
		if !seen {
			frames, err := e.opt.Decoder.Window(e.ctx, c.info.Path, c.info.Rate, 0, 1, 32, 18)
			ok = err == nil && len(frames) == 1
			decodable[ext] = ok
			if !ok {
				what := strings.TrimPrefix(ext, ".") + " files"
				if ext == ".braw" {
					what = "Blackmagic RAW"
				}
				e.notef("%s picture cannot be decoded here; those clips can only be placed by sound.", what)
			}
		}
		c.noPicture = !ok
	}
}

// loadSound computes every clip's loudness contour.
func (e *engine) loadSound(clips []*clipState) error {
	var wg sync.WaitGroup
	errs := make([]error, len(clips))
	total := 0
	for _, c := range clips {
		if c.info.HasAudio {
			total++
		}
	}
	e.begin(StageSound, total, "Listening")
	for i, c := range clips {
		if !c.info.HasAudio {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.limit(func() {
				e.clipState(c, ClipListening)
				c.env, c.wave, errs[i] = e.opt.Decoder.Loudness(e.ctx, c.info.Path, audioRate, audioHop, audioWin)
				e.clipState(c, ClipHeard)
			})
			e.advance()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

const hopSec = float64(audioHop) / audioRate

// soundLag returns how many seconds into a the clip b starts, if their
// sound lines up convincingly.
func soundLag(a, b []float32) (float64, bool) {
	if len(a) < 200 || len(b) < 200 {
		return 0, false
	}
	p, ok := align.Correlate(a, b, false, int(0.5/hopSec))
	if !ok || p.Score <= 0 || p.Score < soundRatio*p.RunnerUp {
		return 0, false
	}
	return p.Offset * hopSec, true
}

// groupBySound merges clips whose sound lines up into groups with a common
// clock, longest clips first.
func (e *engine) groupBySound(clips []*clipState) {
	order := append([]*clipState(nil), clips...)
	sort.SliceStable(order, func(i, j int) bool { return len(order[i].env) > len(order[j].env) })
	var placed []*clipState
	for _, c := range order {
		if c.env == nil {
			continue
		}
		for _, ref := range placed {
			if lag, ok := soundLag(ref.env, c.env); ok {
				c.group, c.pos, c.method = ref.group, ref.pos+lag, BySound
				break
			}
		}
		placed = append(placed, c)
	}
}

// groupByClock attaches clips that sound could not place to a clip of the
// same camera, using the difference between their recording times. A
// camera's clock may be wrong, but it is wrong by the same amount for all of
// its clips.
func (e *engine) groupByClock(clips []*clipState, folders []Folder) {
	size := map[int]int{}
	for _, c := range clips {
		size[c.group]++
	}
	for _, c := range clips {
		if size[c.group] > 1 || c.info.Created.IsZero() {
			continue
		}
		var best *clipState
		for _, o := range clips {
			if o != c && o.folder == c.folder && size[o.group] > 1 && !o.info.Created.IsZero() {
				if best == nil || o.info.Duration.Float() > best.info.Duration.Float() {
					best = o
				}
			}
		}
		if best != nil {
			c.group = best.group
			c.pos = best.pos + c.info.Created.Sub(best.info.Created).Seconds()
			c.method = ByClock
		}
	}
}

// resembles reports whether a folder name looks like a switcher input name,
// which only decides what is tried first.
func resembles(folder, input string) bool {
	f := strings.ToLower(folder)
	for _, w := range strings.FieldsFunc(strings.ToLower(input), func(r rune) bool { return r == ' ' || r == '-' || r == '_' }) {
		if len(w) >= 3 && (strings.HasPrefix(f, w) || strings.HasPrefix(w, f)) {
			return true
		}
	}
	return false
}

// assignInputs works out which switcher input each folder fed, anchoring
// each sound group to the switcher's clock on the way. It returns the source
// index per folder, or -1.
func (e *engine) assignInputs(clips []*clipState, folders []Folder) []int {
	assigned := make([]int, len(folders))
	for i := range assigned {
		assigned[i] = -1
	}
	taken := map[int]bool{}
	cams := e.rec.Cameras()
	for fi, f := range folders {
		if f.Input > 0 {
			assigned[fi], taken[f.Input] = f.Input, true
		}
	}

	// Each folder is represented by its longest clip.
	longest := make([]*clipState, len(folders))
	for _, c := range clips {
		if !c.info.HasVideo || c.noPicture {
			continue
		}
		if l := longest[c.folder]; l == nil || c.info.Duration.Float() > l.info.Duration.Float() {
			longest[c.folder] = c
		}
	}
	// Folders whose name resembles an input go first: they are the likeliest
	// to anchor quickly.
	order := make([]int, 0, len(folders))
	for pass := 0; pass < 2; pass++ {
		for fi, f := range folders {
			named := false
			for _, cam := range cams {
				named = named || resembles(f.Name, cam.Name)
			}
			if named == (pass == 0) {
				order = append(order, fi)
			}
		}
	}

	total := 0
	for _, fi := range order {
		if longest[fi] != nil && folders[fi].Input != NotConnected {
			total++
		}
	}
	e.begin(StageInputs, total, "Finding cameras")
	for _, fi := range order {
		c := longest[fi]
		if c == nil {
			continue
		}
		forced := folders[fi].Input
		if forced == NotConnected {
			continue
		}
		e.clipState(c, ClipSearching)
		e.assignOne(fi, c, folders, cams, forced, taken, assigned)
		e.clipState(c, "")
		e.advance()
	}
	return assigned
}

// assignOne finds the switcher input of one folder, represented by its
// longest clip.
func (e *engine) assignOne(fi int, c *clipState, folders []Folder, cams []atem.Source, forced int, taken map[int]bool, assigned []int) {
	{
		var candidates []atem.Source
		for pass := 0; pass < 2; pass++ {
			for _, cam := range cams {
				if forced > 0 {
					if cam.Index == forced && pass == 0 {
						candidates = append(candidates, cam)
					}
				} else if !taken[cam.Index] && resembles(folders[fi].Name, cam.Name) == (pass == 0) {
					candidates = append(candidates, cam)
				}
			}
		}
		if len(candidates) == 0 {
			return
		}
		e.mu.Lock()
		anchor, anchored := e.anchors[c.group]
		e.mu.Unlock()
		if anchored && forced > 0 {
			return
		}
		if anchored {
			// The group's position is known to a fraction of a second, so a
			// few short windows against each free input settle it.
			e.say("%s: identifying its input", folders[fi].Name)
			if src, ok := e.identify(c, c.pos+anchor, candidates); ok {
				assigned[fi], taken[src] = src, true
			}
			return
		}
		if src, offset, ok := e.search(c, candidates, folders[fi].Name); ok {
			assigned[fi], taken[src] = src, true
			e.mu.Lock()
			e.anchors[c.group] = offset - c.pos
			e.found[c] = true
			e.mu.Unlock()
		}
	}
}

// overlap returns the part of a clip, in clip time, that falls inside the
// recording of an input, given the clip's start on the switcher's clock.
func (e *engine) overlap(c *clipState, start float64, src int) (from, to float64) {
	isoFrom, isoTo := e.iso.span(src)
	from = math.Max(0, isoFrom-start)
	to = math.Min(c.info.Duration.Float(), isoTo-start)
	return
}

// window matches a few seconds of a clip, starting at clip time at, against
// an input's ISO recording. start is the clip's estimated position on the
// switcher's clock and maxLag how far off that estimate may be.
func (e *engine) window(c *clipState, at, start float64, src int, maxLag float64) (float64, align.Peak, bool) {
	rawRate := c.info.Rate.Float()
	pad := maxLag + 0.5
	file, ok := e.iso.locate(src, start+at-pad, windowSec+2*pad)
	if !ok {
		return 0, align.Peak{}, false
	}
	fileStart := file.Start.Float()
	isoRate := file.Media.Rate.Float()
	first := int64(math.Round(at * rawRate))
	isoFirst := int64(math.Round((start + at - pad - fileStart) * isoRate))
	var raw, iso []align.Frame
	var err1, err2 error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		e.limit(func() {
			raw, err1 = e.opt.Decoder.Window(e.ctx, c.info.Path, c.info.Rate, first, int(windowSec*rawRate), 64, 36)
		})
	}()
	go func() {
		defer wg.Done()
		e.limit(func() {
			iso, err2 = e.opt.Decoder.Window(e.ctx, file.Media.Path, file.Media.Rate, isoFirst, int((windowSec+2*pad)*isoRate), 64, 36)
		})
	}()
	wg.Wait()
	if err1 != nil || err2 != nil {
		return 0, align.Peak{}, false
	}
	p, ok := align.Fine(raw, float64(first)/rawRate, rawRate, iso, float64(isoFirst)/isoRate, isoRate, start-fileStart, maxLag)
	if !ok {
		return 0, align.Peak{}, false
	}
	return p.Offset + fileStart, p, p.Score >= fineScore && p.Lead() >= fineLead
}

// identify finds which of the candidate inputs a clip's picture matches,
// given its estimated start on the switcher's clock.
func (e *engine) identify(c *clipState, start float64, candidates []atem.Source) (int, bool) {
	for _, frac := range []float64{0.5, 0.25, 0.75} {
		type hit struct {
			src   int
			score float64
		}
		hits := make([]hit, len(candidates))
		var wg sync.WaitGroup
		for i, cam := range candidates {
			from, to := e.overlap(c, start, cam.Index)
			if to-from < windowSec+4 {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				at := from + 2 + (to-from-windowSec-4)*frac
				if _, p, ok := e.window(c, at, start, cam.Index, 1.5); ok {
					hits[i] = hit{cam.Index, p.Score}
				}
			}()
		}
		wg.Wait()
		best := hit{}
		for _, h := range hits {
			if h.score > best.score {
				best = h
			}
		}
		if best.score > 0 {
			return best.src, true
		}
	}
	return 0, false
}

// track returns a stretch of an ISO file sampled once a second, cached.
func (e *engine) track(file ISOFile, from, length float64) []align.Frame {
	key := fmt.Sprintf("%s|%.0f|%.0f", file.Media.Path, from, length)
	e.mu.Lock()
	t, ok := e.tracks[key]
	e.mu.Unlock()
	if ok {
		return t
	}
	e.limit(func() {
		t, _ = e.opt.Decoder.Track(e.ctx, file.Media.Path, from, length, 1, 32, 18)
	})
	e.mu.Lock()
	e.tracks[key] = t
	e.mu.Unlock()
	return t
}

// search finds a clip in the ISO recordings with no prior idea of where it
// is, widening the search until it is found. It returns the input and the
// clip's start on the switcher's clock.
func (e *engine) search(c *clipState, candidates []atem.Source, name string) (int, float64, bool) {
	dur := c.info.Duration.Float()
	rawRate := c.info.Rate.Float()
	const n = 24
	// Single frames spread over the first minutes of the clip.
	sample := func(span float64) ([]align.Frame, []float64) {
		span = math.Min(span, dur-1)
		frames := make([]align.Frame, n)
		times := make([]float64, n)
		var wg sync.WaitGroup
		for k := range frames {
			first := int64((0.5 + (span-1)*float64(k)/float64(n-1)) * rawRate)
			times[k] = float64(first) / rawRate
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.limit(func() {
					if f, err := e.opt.Decoder.Window(e.ctx, c.info.Path, c.info.Rate, first, 1, 32, 18); err == nil && len(f) == 1 {
						frames[k] = f[0]
					}
				})
			}()
		}
		wg.Wait()
		var fs []align.Frame
		var ts []float64
		for k, f := range frames {
			if f != nil {
				fs, ts = append(fs, f), append(ts, times[k])
			}
		}
		return fs, ts
	}

	// Each stage looks at more of the clip and more of the ISO recording.
	// The camera usually starts within a few minutes of the switcher, so the
	// first stage is the common case.
	type stage struct{ rawSpan, isoLen float64 }
	for _, st := range []stage{{300, 900}, {1200, 3600}, {dur, math.Inf(1)}} {
		if math.IsInf(st.isoLen, 1) {
			e.say("%s: searching the whole recording", name)
		} else {
			e.say("%s: searching the first %.0f min", name, st.isoLen/60)
		}
		samples, times := sample(st.rawSpan)
		if len(samples) < n/2 {
			return 0, 0, false
		}
		type found struct {
			src    int
			offset float64
			peak   align.Peak
			ok     bool
		}
		results := make([]found, len(candidates))
		var wg sync.WaitGroup
		for i, cam := range candidates {
			files := e.iso.Files[cam.Index]
			if len(files) == 0 {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				file := files[0]
				length := math.Min(st.isoLen, file.Media.Duration.Float())
				track := e.track(file, 0, length)
				p, ok := align.Coarse(samples, times, track, 0, 1)
				if ok && p.Score >= coarseScore && p.Lead() >= coarseLead {
					results[i] = found{cam.Index, p.Offset + file.Start.Float(), p, true}
				}
			}()
		}
		wg.Wait()
		best := found{}
		for _, r := range results {
			if r.ok && (!best.ok || r.peak.Score > best.peak.Score) {
				best = r
			}
		}
		if !best.ok {
			continue
		}
		// Confirm frame by frame before trusting it.
		from, to := e.overlap(c, best.offset, best.src)
		for _, frac := range []float64{0.3, 0.6, 0.1} {
			at := from + 3 + (math.Min(to, from+st.rawSpan)-from-windowSec-6)*frac
			if off, _, ok := e.window(c, at, best.offset, best.src, 2.5); ok {
				return best.src, off, true
			}
		}
	}
	return 0, 0, false
}

// match is one frame-by-frame result: the offset found at a time into the clip.
type match struct{ at, offset float64 }

// finish settles one clip's final position.
func (e *engine) finish(c *clipState, src int) {
	r := Clip{Media: c.info, Wave: c.wave}
	defer func() { c.result = r }()
	e.mu.Lock()
	anchor, anchored := e.anchors[c.group]
	e.mu.Unlock()
	if !anchored {
		r.Warning = "not found in the switcher recordings, and its sound matches no other clip"
		if c.noPicture {
			r.Warning = "picture cannot be decoded, and no clip sharing its sound was found in the switcher recordings"
		}
		return
	}
	est := c.pos + anchor
	r.Placed, r.Offset, r.Method = true, est, c.method
	if r.Method == "" {
		// The clip the others were lined up against has no method of its
		// own: it is placed by picture if the search found it, and otherwise
		// through the sound it shares with the clip that was found.
		r.Method = BySound
		e.mu.Lock()
		if e.found[c] {
			r.Method = ByPicture
		}
		e.mu.Unlock()
	}
	if src < 0 || !c.info.HasVideo || c.noPicture {
		if c.method == ByClock {
			r.Warning = "placed by camera clock, may be seconds off"
		}
		return
	}

	// Frame-by-frame windows across the part that overlaps the recording.
	from, to := e.overlap(c, est, src)
	usable := to - from - windowSec - 4
	if usable <= 0 {
		if c.method == ByClock {
			r.Warning = "placed by camera clock, may be seconds off"
		}
		return
	}
	fracs := []float64{0.5}
	switch {
	case usable > 600:
		fracs = []float64{0.03, 0.27, 0.5, 0.73, 0.97}
	case usable > 30:
		fracs = []float64{0.1, 0.9}
	}
	maxLag := 1.0
	if c.method == ByClock {
		maxLag = 3.0
	}
	matches := make([]*match, len(fracs))
	var wg sync.WaitGroup
	for i, frac := range fracs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A static shot gives nothing to match; step along and retry.
			for try := 0.0; try < 3; try++ {
				at := from + 2 + usable*frac + try*15*math.Copysign(1, 0.5-frac)
				if at < from || at > to-windowSec {
					break
				}
				if off, _, ok := e.window(c, at, est, src, maxLag); ok {
					matches[i] = &match{at, off}
					return
				}
			}
		}()
	}
	wg.Wait()
	var got []match
	for _, m := range matches {
		if m != nil {
			got = append(got, *m)
		}
	}
	if len(got) == 0 {
		switch c.method {
		case ByClock:
			r.Warning = "picture match failed; placed by camera clock, may be seconds off"
		default:
			r.Warning = "picture match failed; placed by sound, may be a few frames off"
		}
		return
	}
	offsets := make([]float64, len(got))
	for i, m := range got {
		offsets[i] = m.offset
	}
	sort.Float64s(offsets)
	r.Offset, r.Method, r.Windows = offsets[len(offsets)/2], ByPicture, len(got)
	if len(got) < 3 {
		if len(got) == 2 {
			r.Drift = got[1].offset - got[0].offset
		}
		return
	}
	// With several matches along the clip, fit a straight line: the offset
	// at the start plus a steady drift.
	slope, intercept := fitLine(got)
	r.Drift = slope * (to - from)
	r.Offset = intercept + slope*(from+to)/2
	frame := 1 / c.info.Rate.Float()
	steps := int(math.Round(math.Abs(r.Drift) / frame))
	if steps == 0 {
		return
	}
	// Place the clip as steps+1 parts. Each part overlaps the next by a few
	// frames so that rounding to whole frames can never leave a hole; the
	// later part simply starts where the earlier one ends.
	dur := c.info.Duration.Float()
	n := steps + 1
	for i := 0; i < n; i++ {
		a := from + (to-from)*float64(i)/float64(n)
		b := from + (to-from)*float64(i+1)/float64(n)
		part := Part{In: a, Out: b + 3*frame, Offset: intercept + slope*(a+b)/2}
		if i == 0 {
			part.In = 0
		}
		if i == n-1 || part.Out > dur {
			part.Out = dur
		}
		r.Parts = append(r.Parts, part)
	}
}

// fitLine returns the least-squares line through the matches: offset as a
// function of the time into the clip.
func fitLine(m []match) (slope, intercept float64) {
	var sx, sy, sxx, sxy float64
	for _, p := range m {
		sx += p.at
		sy += p.offset
		sxx += p.at * p.at
		sxy += p.at * p.offset
	}
	n := float64(len(m))
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, sy / n
	}
	slope = (n*sxy - sx*sy) / den
	return slope, (sy - slope*sx) / n
}

// placeMaster lines the separately recorded mix up with the cameras.
func (e *engine) placeMaster(m *clipState, clips []*clipState) *Clip {
	r := &Clip{Media: m.info, Wave: m.wave}
	if m.env == nil {
		r.Warning = "has no audio"
		return r
	}
	// A microphone hears the PA later the further away it is, so lining the
	// mix up with a distant camera places it late. Of all the cameras that
	// match, the one that puts the mix earliest is the closest to the truth.
	found := false
	for _, c := range clips {
		if c.env == nil || !c.result.Placed {
			continue
		}
		lag, ok := soundLag(c.env, m.env)
		if !ok {
			continue
		}
		if off := c.result.Offset + lag; !found || off < r.Offset {
			r.Offset = off
		}
		found = true
	}
	if !found {
		r.Warning = "its sound matches none of the cameras"
		return r
	}
	r.Placed, r.Method = true, BySound
	return r
}
