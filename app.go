package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"

	"resolve-sync/align"
	"resolve-sync/assemble"
	"resolve-sync/atem"
	"resolve-sync/media"
	"resolve-sync/project"
	"resolve-sync/syncer"
	"resolve-sync/timebase"
)

// App is what the window can call.
type App struct {
	ctx    context.Context
	app    *application.App
	win    *application.WebviewWindow
	prober media.Prober
	dec    align.Decoder

	mu      sync.Mutex
	drp     string
	rec     *atem.Recording
	iso     *syncer.ISOSet
	folders map[string]syncer.Folder // scanned, by path
	result  *syncer.Result
	cancel  context.CancelFunc
	thumbs  map[string]string // data URL by clip path
	thumbQ  chan struct{}     // limits how many thumbnails are made at once
}

func NewApp() *App {
	return &App{folders: map[string]syncer.Folder{}, thumbs: map[string]string{}, thumbQ: make(chan struct{}, 3)}
}

// ServiceStartup is called by Wails before the window opens.
func (a *App) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	a.ctx = ctx
	a.prober = media.Prober{FFprobe: project.FindTool("ffprobe")}
	a.dec = align.Decoder{FFmpeg: project.FindTool("ffmpeg"), BRAW: project.FindBRAW()}
	return nil
}

func (a *App) emit(name string, data any) {
	if a.app != nil {
		a.app.Event.Emit(name, data)
	}
}

type Input struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
}

type ProjectInfo struct {
	Path   string  `json:"path"`
	Name   string  `json:"name"`
	Detail string  `json:"detail"`
	Inputs []Input `json:"inputs"`
}

type FolderInfo struct {
	Path   string     `json:"path"`
	Name   string     `json:"name"`
	Detail string     `json:"detail"`
	Files  []FileInfo `json:"files"`
}

// FileInfo is one clip in a folder.
type FileInfo struct {
	Path     string  `json:"path"`
	Name     string  `json:"name"`
	Duration float64 `json:"duration"`
	Thumb    string  `json:"thumb"` // data URL, if already made; otherwise it arrives as a "thumb" event
}

// Thumb is a finished thumbnail.
type Thumb struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

type MixInfo struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// Opened is what came of a set of paths the user chose or dropped.
type Opened struct {
	Project *ProjectInfo `json:"project"`
	Folders []FolderInfo `json:"folders"`
	Mix     *MixInfo     `json:"mix"`
	Errors  []string     `json:"errors"`
}

func clock(seconds float64) string {
	s := int(seconds + 0.5)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}

var audioExt = map[string]bool{".wav": true, ".aif": true, ".aiff": true, ".flac": true, ".mp3": true, ".m4a": true}

// Open takes any mix of a project file, camera folders and a PA mix.
func (a *App) Open(paths []string) Opened {
	var out Opened
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			out.Errors = append(out.Errors, err.Error())
			continue
		}
		ext := strings.ToLower(filepath.Ext(p))
		switch {
		case st.IsDir():
			f, err := project.ScanFolder(a.ctx, a.prober, p)
			if err != nil {
				out.Errors = append(out.Errors, err.Error())
				continue
			}
			var total float64
			for _, c := range f.Clips {
				total += c.Duration.Float()
			}
			a.mu.Lock()
			a.folders[p] = f
			a.mu.Unlock()
			noun := "clips"
			if len(f.Clips) == 1 {
				noun = "clip"
			}
			info := FolderInfo{Path: p, Name: f.Name, Detail: fmt.Sprintf("%d %s · %s", len(f.Clips), noun, clock(total))}
			for _, c := range f.Clips {
				a.mu.Lock()
				thumb := a.thumbs[c.Path]
				a.mu.Unlock()
				info.Files = append(info.Files, FileInfo{Path: c.Path, Name: filepath.Base(c.Path), Duration: c.Duration.Float(), Thumb: thumb})
				if thumb == "" {
					go a.makeThumb(c)
				}
			}
			out.Folders = append(out.Folders, info)
		case ext == ".drp":
			info, err := a.loadProject(p)
			if err != nil {
				out.Errors = append(out.Errors, err.Error())
				continue
			}
			out.Project = info
		case audioExt[ext]:
			out.Mix = &MixInfo{Path: p, Name: filepath.Base(p)}
		default:
			out.Errors = append(out.Errors, filepath.Base(p)+": not a project file, a folder or an audio file")
		}
	}
	return out
}

// makeThumb makes a small picture from 40% into a clip and sends it to the
// window as a "thumb" event.
func (a *App) makeThumb(c media.Info) {
	a.thumbQ <- struct{}{}
	defer func() { <-a.thumbQ }()
	const w = 160
	h := 90
	if c.Width > 0 && c.Height > 0 {
		h = max(2, int(float64(w)*float64(c.Height)/float64(c.Width)+0.5)&^1)
	}
	img, err := a.dec.Thumbnail(a.ctx, c.Path, c.Rate, max(1, c.Duration.Float()*0.4), w, h)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 82}); err != nil {
		return
	}
	url := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	a.mu.Lock()
	a.thumbs[c.Path] = url
	a.mu.Unlock()
	a.emit("thumb", Thumb{Path: c.Path, URL: url})
}

func (a *App) loadProject(path string) (*ProjectInfo, error) {
	rec, err := atem.ParseFile(path)
	if err != nil {
		return nil, err
	}
	iso, err := syncer.LoadISO(a.ctx, a.prober, rec, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.drp, a.rec, a.iso, a.result = path, rec, iso, nil
	a.mu.Unlock()
	info := &ProjectInfo{
		Path: path,
		Name: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Detail: fmt.Sprintf("%s · %s · %d cuts", rec.VideoMode,
			clock(timebase.FrameTime(rec.Duration, iso.Rate).Float()), len(rec.Program)-1),
	}
	for _, c := range rec.Cameras() {
		info.Inputs = append(info.Inputs, Input{c.Index, c.Name})
	}
	return info, nil
}

func (a *App) openDialog(title string) *application.OpenFileDialogStruct {
	d := a.app.Dialog.OpenFile().SetTitle(title).CanCreateDirectories(false)
	if a.win != nil {
		d.AttachToWindow(a.win)
	}
	return d
}

func (a *App) PickProject() Opened {
	p, err := a.openDialog("ATEM project").CanChooseFiles(true).
		AddFilter("ATEM project (*.drp)", "*.drp").PromptForSingleSelection()
	if err != nil || p == "" {
		return Opened{}
	}
	return a.Open([]string{p})
}

// PickFolder lets the user choose several camera folders at once.
func (a *App) PickFolder() Opened {
	ps, err := a.openDialog("Camera folders").CanChooseFiles(false).CanChooseDirectories(true).
		PromptForMultipleSelection()
	if err != nil || len(ps) == 0 {
		return Opened{}
	}
	return a.Open(ps)
}

func (a *App) PickMix() Opened {
	p, err := a.openDialog("PA mix").CanChooseFiles(true).
		AddFilter("Audio", "*.wav;*.aif;*.aiff;*.flac;*.mp3;*.m4a").PromptForSingleSelection()
	if err != nil || p == "" {
		return Opened{}
	}
	return a.Open([]string{p})
}

type FolderRequest struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Input int    `json:"input"` // 0 auto, -1 not connected, else source index
}

type SyncRequest struct {
	Folders []FolderRequest `json:"folders"`
	Mix     string          `json:"mix"`
}

type ClipRow struct {
	Path     string  `json:"path"`
	Duration float64 `json:"duration"`
	Wave     []byte  `json:"wave"` // sound level, one value a second (base64 in JSON)
	File     string  `json:"file"`
	Placed   bool    `json:"placed"`
	Offset   float64 `json:"offset"`
	Method   string  `json:"method"`
	Exact    bool    `json:"exact"` // matched frame by frame
	Warning  string  `json:"warning"`
}

type AngleRow struct {
	Path  string    `json:"path"`
	Input int       `json:"input"`
	Clips []ClipRow `json:"clips"`
}

type SyncView struct {
	Angles []AngleRow `json:"angles"`
	Mix    *ClipRow   `json:"mix"`
	Notes  []string   `json:"notes"`
	// Length of the switcher recording and what it had on air, in seconds
	// from its start; Angle is an index into Angles, or -1.
	Length  float64      `json:"length"`
	Program []ProgramRow `json:"program"`
}

type ProgramRow struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Angle int     `json:"angle"`
}

func row(c syncer.Clip) ClipRow {
	r := ClipRow{Path: c.Media.Path, Duration: c.Media.Duration.Float(), Wave: c.Wave,
		File: filepath.Base(c.Media.Path), Placed: c.Placed, Offset: c.Offset, Method: c.Method, Exact: c.Method == syncer.ByPicture, Warning: c.Warning}
	if n := len(c.Parts); n > 1 {
		r.Method = fmt.Sprintf("%s · %d parts", c.Method, n)
	}
	return r
}

// Sync places the footage. Progress arrives as "progress" events.
func (a *App) Sync(req SyncRequest) (*SyncView, error) {
	a.mu.Lock()
	rec, iso := a.rec, a.iso
	var folders []syncer.Folder
	for _, f := range req.Folders {
		scanned, ok := a.folders[f.Path]
		if !ok {
			a.mu.Unlock()
			return nil, fmt.Errorf("%s was not opened", f.Path)
		}
		scanned.Input = f.Input
		if strings.TrimSpace(f.Name) != "" {
			scanned.Name = strings.TrimSpace(f.Name)
		}
		folders = append(folders, scanned)
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.cancel = cancel
	a.mu.Unlock()
	defer cancel()
	if rec == nil {
		return nil, fmt.Errorf("choose an ATEM project first")
	}
	if len(folders) == 0 {
		return nil, fmt.Errorf("add at least one camera folder")
	}
	opt := syncer.Options{Decoder: a.dec, Progress: func(p syncer.Progress) { a.emit("progress", p) }}
	if req.Mix != "" {
		info, err := a.prober.Probe(ctx, req.Mix)
		if err != nil {
			return nil, err
		}
		opt.MasterAudio = &info
	}
	res, err := syncer.Run(ctx, rec, iso, folders, opt)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("cancelled")
	}
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.result = res
	a.mu.Unlock()

	view := &SyncView{Notes: res.Notes, Length: timebase.FrameTime(rec.Duration, iso.Rate).Float()}
	angleOf := map[int]int{}
	for i, ang := range res.Angles {
		if ang.Source > 0 {
			angleOf[ang.Source] = i
		}
	}
	for _, seg := range rec.Program {
		angle, ok := angleOf[seg.Source]
		if !ok {
			angle = -1
		}
		start, end := timebase.FrameTime(seg.Start, iso.Rate).Float(), timebase.FrameTime(seg.End, iso.Rate).Float()
		if n := len(view.Program); n > 0 && view.Program[n-1].Angle == angle {
			view.Program[n-1].End = end
			continue
		}
		view.Program = append(view.Program, ProgramRow{Start: start, End: end, Angle: angle})
	}
	for i, ang := range res.Angles {
		r := AngleRow{Path: req.Folders[i].Path, Input: ang.Source}
		for _, c := range ang.Clips {
			r.Clips = append(r.Clips, row(c))
		}
		view.Angles = append(view.Angles, r)
	}
	if res.Master != nil {
		m := row(*res.Master)
		view.Mix = &m
	}
	return view, nil
}

// Cancel stops a running sync.
func (a *App) Cancel() {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
}

type SaveRequest struct {
	Names []string `json:"names"`
	// Audio is "" for audio that follows the picture, "mix" for the PA mix,
	// or the index of an angle.
	Audio   string `json:"audio"`
	MinShot int64  `json:"minShot"`
}

type SaveView struct {
	Path    string   `json:"path"`
	Summary string   `json:"summary"`
	Gaps    []string `json:"gaps"`
	Notes   []string `json:"notes"`
}

// Save asks where to write the timeline and writes it. It returns nil if
// the user cancels.
func (a *App) Save(req SaveRequest) (*SaveView, error) {
	a.mu.Lock()
	rec, iso, res, drp := a.rec, a.iso, a.result, a.drp
	a.mu.Unlock()
	if res == nil {
		return nil, fmt.Errorf("sync first")
	}
	angles, mix, placed := project.Angles(res, req.Names)
	if placed == 0 {
		return nil, fmt.Errorf("no clip could be placed, so there is nothing to save")
	}
	name := strings.TrimSuffix(filepath.Base(drp), filepath.Ext(drp))
	opt := assemble.Options{Name: name, ATEMRate: iso.Rate, AudioAngle: -1, MinShotFrames: req.MinShot}
	switch req.Audio {
	case "":
	case "mix":
		if mix < 0 {
			return nil, fmt.Errorf("the PA mix could not be placed; choose another audio source")
		}
		opt.AudioAngle = mix
	default:
		var i int
		if _, err := fmt.Sscanf(req.Audio, "%d", &i); err != nil || i < 0 || i >= len(res.Angles) {
			return nil, fmt.Errorf("unknown audio source")
		}
		opt.AudioAngle = i
	}
	proj, rep, err := assemble.Build(rec, angles, opt)
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	d := a.app.Dialog.SaveFile().SetDirectory(filepath.Join(home, "Movies")).SetFilename(name+".fcpxml").
		AddFilter("FCPXML (*.fcpxml)", "*.fcpxml")
	if a.win != nil {
		d.AttachToWindow(a.win)
	}
	path, err := d.PromptForSingleSelection()
	if err != nil || path == "" {
		return nil, err
	}
	if err := project.Write(path, proj); err != nil {
		return nil, err
	}
	view := &SaveView{Path: path, Notes: rep.Notes,
		Summary: fmt.Sprintf("%.5g fps · %s · %d cuts · %d dissolves", rep.Rate.Float(), rep.Timecode(rep.Frames), rep.Cuts, rep.Dissolves)}
	for _, g := range rep.OnAirGaps {
		view.Gaps = append(view.Gaps, fmt.Sprintf("%s  %s – %s", g.Angle, rep.Timecode(g.Start), rep.Timecode(g.End)))
	}
	return view, nil
}

// Ready is called by the window once it has loaded; the log line makes a
// broken connection between the window and the app easy to spot.
func (a *App) Ready() {
	fmt.Fprintln(os.Stderr, "window connected; ffmpeg:", a.dec.FFmpeg, "ffprobe:", a.prober.FFprobe, "braw:", a.dec.BRAW)
}
