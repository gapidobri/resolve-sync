// Command resolve-sync is a thin command-line front end to the core packages.
// The GUI will call the same packages.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"resolve-sync/align"
	"resolve-sync/assemble"
	"resolve-sync/atem"
	"resolve-sync/media"
	"resolve-sync/project"
	"resolve-sync/syncer"
	"resolve-sync/timebase"
)

const usage = `Usage:
  resolve-sync inspect <project.drp>
      Print what the ATEM project file contains.

  resolve-sync build [flags] <project.drp> <camera folder>...
      Sync the raw footage in each folder to the switcher recording and
      write a multicam timeline. Each folder becomes one angle, named after
      the folder. The footage is only read; the output goes to the current
      folder unless -o says otherwise.

  resolve-sync iso [flags] <project.drp>
      Build a multicam timeline from the switcher's own ISO recordings.
      This needs no syncing and exists to test the Resolve import at full
      scale; the real output uses the raw camera footage instead.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "inspect":
		err = inspect(os.Args[2:])
	case "iso":
		err = iso(os.Args[2:])
	case "build":
		err = build(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func inspect(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("inspect takes one project file")
	}
	rec, err := atem.ParseFile(args[0])
	if err != nil {
		return err
	}
	nominal := timebase.Nominal(rec.Rate)
	fmt.Printf("Video mode   %s (%.3f fps)\n", rec.VideoMode, rec.Rate.Float())
	fmt.Printf("Started at   %s\n", timebase.FormatTimecode(rec.StartTC, nominal))
	fmt.Printf("Length       %s\n", timebase.FormatTimecode(rec.Duration, nominal))
	fmt.Printf("Cuts         %d, of which %d are transitions\n", len(rec.Program)-1, len(rec.Transitions))
	onAir := map[int]int64{}
	for _, s := range rec.Program {
		onAir[s.Source] += s.End - s.Start
	}
	fmt.Println("\nInputs")
	for _, s := range rec.Sources {
		if !s.IsCamera() && onAir[s.Index] == 0 {
			continue
		}
		fmt.Printf("  %2d  %-16s on air %s", s.Index, s.Name, timebase.FormatTimecode(onAir[s.Index], nominal))
		if len(s.Files) == 0 {
			fmt.Printf("  (%s, no recording)", s.Type)
		}
		fmt.Println()
		for _, f := range s.Files {
			fmt.Printf("        from %s  %s\n", timebase.FormatTimecode(f.Start, nominal), f.File)
		}
	}
	if len(rec.Ignored) > 0 {
		fmt.Println("\nNot translated")
		for what, n := range rec.Ignored {
			fmt.Printf("  %s (%d)\n", what, n)
		}
	}
	return nil
}

func iso(args []string) error {
	fs := flag.NewFlagSet("iso", flag.ContinueOnError)
	mediaDir := fs.String("media", "", "folder containing \"Video ISO Files\" (default: the project file's folder)")
	out := fs.String("o", "", "output file (default: <project>-iso.fcpxml in the current folder; never written next to the footage)")
	audio := fs.Int("audio", 0, "switcher input whose audio plays throughout; 0 lets audio follow the picture")
	minShot := fs.Int64("min-shot", 0, "remove shots shorter than this many timeline frames; the shot before carries on")
	ffprobe := fs.String("ffprobe", project.FindTool("ffprobe"), "path to ffprobe")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("iso takes one project file")
	}
	drp := fs.Arg(0)
	rec, err := atem.ParseFile(drp)
	if err != nil {
		return err
	}
	dir := *mediaDir
	if dir == "" {
		dir = filepath.Dir(drp)
	}
	name := strings.TrimSuffix(filepath.Base(drp), filepath.Ext(drp))
	if *out == "" {
		*out = name + "-iso.fcpxml"
	}

	prober := media.Prober{FFprobe: *ffprobe}
	ctx := context.Background()
	opt := assemble.Options{Name: name + " ISO", AudioAngle: -1, MinShotFrames: *minShot}
	set, err := syncer.LoadISO(ctx, prober, rec, dir)
	if err != nil {
		return err
	}
	if set.Rate != rec.Rate {
		fmt.Printf("Note: the project file says %s but the ISO files run at %.3f fps; using the ISO rate.\n", rec.VideoMode, set.Rate.Float())
	}
	opt.ATEMRate = set.Rate
	var angles []assemble.Angle
	for _, cam := range rec.Cameras() {
		if cam.Index == *audio {
			opt.AudioAngle = len(angles)
		}
		angle := assemble.Angle{Name: cam.Name, Source: cam.Index}
		for _, f := range set.Files[cam.Index] {
			angle.Clips = append(angle.Clips, assemble.PlacedClip{Media: f.Media, Offset: f.Start})
		}
		angles = append(angles, angle)
	}
	if *audio != 0 && opt.AudioAngle < 0 {
		return fmt.Errorf("input %d is not a recorded camera", *audio)
	}

	proj, rep, err := assemble.Build(rec, angles, opt)
	if err != nil {
		return err
	}
	if err := project.Write(*out, proj); err != nil {
		return err
	}
	printReport(rep)
	fmt.Printf("\nWrote %s\n", *out)
	return nil
}

func build(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	mediaDir := fs.String("media", "", "folder containing \"Video ISO Files\" (default: the project file's folder)")
	out := fs.String("o", "", "output file (default: <project>.fcpxml in the current folder)")
	audio := fs.String("audio", "", "name of the camera folder whose audio plays throughout (default: audio follows the picture, or the mix if -mix is given)")
	mix := fs.String("mix", "", "a separate recording of the PA mix to sync and use as the timeline's audio")
	minShot := fs.Int64("min-shot", 0, "remove shots shorter than this many timeline frames; the shot before carries on")
	ffprobe := fs.String("ffprobe", project.FindTool("ffprobe"), "path to ffprobe")
	ffmpeg := fs.String("ffmpeg", project.FindTool("ffmpeg"), "path to ffmpeg")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("build takes a project file and at least one camera folder")
	}
	started := time.Now()
	drp := fs.Arg(0)
	rec, err := atem.ParseFile(drp)
	if err != nil {
		return err
	}
	dir := *mediaDir
	if dir == "" {
		dir = filepath.Dir(drp)
	}
	name := strings.TrimSuffix(filepath.Base(drp), filepath.Ext(drp))
	if *out == "" {
		*out = name + ".fcpxml"
	}
	ctx := context.Background()
	prober := media.Prober{FFprobe: *ffprobe}
	set, err := syncer.LoadISO(ctx, prober, rec, dir)
	if err != nil {
		return err
	}
	var folders []syncer.Folder
	for _, d := range fs.Args()[1:] {
		f, err := project.ScanFolder(ctx, prober, d)
		if err != nil {
			return err
		}
		folders = append(folders, f)
	}
	sopt := syncer.Options{Decoder: align.Decoder{FFmpeg: *ffmpeg, BRAW: project.FindBRAW()}, Progress: printProgress()}
	if *mix != "" {
		info, err := prober.Probe(ctx, *mix)
		if err != nil {
			return err
		}
		sopt.MasterAudio = &info
	}
	fmt.Println("Syncing")
	res, err := syncer.Run(ctx, rec, set, folders, sopt)
	if err != nil {
		return err
	}

	opt := assemble.Options{Name: name, ATEMRate: set.Rate, AudioAngle: -1, MinShotFrames: *minShot}
	fmt.Println("\nResult")
	for _, a := range res.Angles {
		input := "not on the switcher"
		if src, ok := rec.Source(a.Source); ok && a.Source >= 0 {
			input = fmt.Sprintf("input %d %q", src.Index, src.Name)
		}
		fmt.Printf("  %s  (%s)\n", a.Name, input)
		for _, c := range a.Clips {
			printClip(c)
		}
	}
	if res.Master != nil {
		fmt.Println("  " + project.MixAngle)
		printClip(*res.Master)
	}
	for _, n := range res.Notes {
		fmt.Println("  " + n)
	}
	angles, mixAngle, placed := project.Angles(res, nil)
	if placed == 0 {
		return fmt.Errorf("no clip could be placed on the switcher's timeline; nothing was written")
	}
	if *audio != "" {
		for i, a := range angles {
			if strings.EqualFold(a.Name, *audio) {
				opt.AudioAngle = i
			}
		}
		if opt.AudioAngle < 0 {
			return fmt.Errorf("-audio %q is not one of the camera folders", *audio)
		}
	} else {
		opt.AudioAngle = mixAngle
	}

	proj, rep, err := assemble.Build(rec, angles, opt)
	if err != nil {
		return err
	}
	if err := project.Write(*out, proj); err != nil {
		return err
	}
	fmt.Println()
	printReport(rep)
	fmt.Printf("\nWrote %s in %.0f s\n", *out, time.Since(started).Seconds())
	return nil
}

// printProgress prints each new stage description once.
func printProgress() func(syncer.Progress) {
	var mu sync.Mutex
	last := ""
	return func(p syncer.Progress) {
		mu.Lock()
		defer mu.Unlock()
		if p.Text != "" && p.Text != last {
			fmt.Println("  " + p.Text)
			last = p.Text
		}
	}
}

func printClip(c syncer.Clip) {
	line := "not placed"
	if c.Placed {
		line = fmt.Sprintf("starts %+10.3f s  by %-7s", c.Offset, c.Method)
		if c.Windows > 0 {
			line += fmt.Sprintf("  %d windows, drift %+.0f ms", c.Windows, c.Drift*1000)
			if n := len(c.Parts); n > 1 {
				line += fmt.Sprintf(", corrected in %d parts", n)
			}
		}
	}
	fmt.Printf("    %-28s %s\n", filepath.Base(c.Media.Path), line)
	if c.Warning != "" {
		fmt.Printf("      ! %s\n", c.Warning)
	}
}

func printReport(rep *assemble.Report) {
	fmt.Printf("Timeline     %.3f fps, %s long\n", rep.Rate.Float(), rep.Timecode(rep.Frames))
	fmt.Printf("Cuts         %d, of which %d are dissolves\n", rep.Cuts, rep.Dissolves)
	if len(rep.OnAirGaps) > 0 {
		fmt.Printf("\nOn air without footage (black in the timeline): %d stretches\n", len(rep.OnAirGaps))
		for _, g := range rep.OnAirGaps {
			fmt.Printf("  %-16s %s - %s\n", g.Angle, rep.Timecode(g.Start), rep.Timecode(g.End))
		}
	}
	if len(rep.Unused) > 0 {
		fmt.Printf("\nOutside the recording, not used: %d clips\n", len(rep.Unused))
		for _, p := range rep.Unused {
			fmt.Println("  " + p)
		}
	}
	if len(rep.Notes) > 0 {
		fmt.Println("\nNotes")
		for _, n := range rep.Notes {
			fmt.Println("  " + n)
		}
	}
}
