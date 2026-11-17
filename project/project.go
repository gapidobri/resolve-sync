// Package project holds the steps shared by the command line and the GUI:
// finding footage in a folder and turning a sync result into angles.
package project

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"resolve-sync/assemble"
	"resolve-sync/fcpxml"
	"resolve-sync/media"
	"resolve-sync/syncer"
	"resolve-sync/timebase"
)

var videoExt = map[string]bool{".mp4": true, ".mov": true, ".mxf": true, ".mts": true, ".m4v": true, ".avi": true, ".braw": true}

// ScanFolder probes the video files directly inside a folder. Subfolders
// (proxies, thumbnails) are not entered.
func ScanFolder(ctx context.Context, prober media.Prober, dir string) (syncer.Folder, error) {
	folder := syncer.Folder{Name: filepath.Base(filepath.Clean(dir))}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return folder, err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !videoExt[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		info, err := prober.Probe(ctx, filepath.Join(dir, e.Name()))
		if err != nil {
			return folder, err
		}
		if info.HasVideo {
			folder.Clips = append(folder.Clips, info)
		}
	}
	if len(folder.Clips) == 0 {
		return folder, fmt.Errorf("%s: no video files", dir)
	}
	return folder, nil
}

// MixAngle is the name given to the angle that carries a separate PA mix.
const MixAngle = "PA mix"

// Angles converts a sync result into angles for assembly. names, if not
// nil, renames the angles in order. It returns the index of the PA mix
// angle, or -1 if there is none or it could not be placed.
func Angles(res *syncer.Result, names []string) (angles []assemble.Angle, mix int, placed int) {
	mix = -1
	for i, a := range res.Angles {
		angle := assemble.Angle{Name: a.Name, Source: a.Source}
		if i < len(names) && strings.TrimSpace(names[i]) != "" {
			angle.Name = strings.TrimSpace(names[i])
		}
		if a.Source < 0 {
			angle.Source = assemble.NoSource
		}
		for _, c := range a.Clips {
			if !c.Placed {
				continue
			}
			placed++
			if len(c.Parts) == 0 {
				angle.Clips = append(angle.Clips, assemble.PlacedClip{Media: c.Media, Offset: seconds(c.Offset)})
			}
			for _, part := range c.Parts {
				angle.Clips = append(angle.Clips, assemble.PlacedClip{Media: c.Media, Offset: seconds(part.Offset), In: seconds(part.In), Out: seconds(part.Out)})
			}
		}
		angles = append(angles, angle)
	}
	if m := res.Master; m != nil && m.Placed {
		mix = len(angles)
		angles = append(angles, assemble.Angle{Name: MixAngle, Source: assemble.NoSource,
			Clips: []assemble.PlacedClip{{Media: m.Media, Offset: seconds(m.Offset)}}})
	}
	return angles, mix, placed
}

// seconds converts a measured offset to an exact value, to the microsecond.
func seconds(s float64) timebase.Rat {
	return timebase.New(int64(math.Round(s*1e6)), 1e6)
}

// Write writes the timeline, leaving no partial file behind on failure.
func Write(path string, p *fcpxml.Project) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := fcpxml.Write(f, p); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// FindTool locates ffmpeg or ffprobe. An app started from the Finder does
// not see the shell's PATH, so the usual install locations are checked too,
// after any copy shipped next to the executable.
func FindTool(name string) string {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, name), filepath.Join(dir, "..", "Resources", name),
			filepath.Join(dir, "tools", "bin", name), filepath.Join(dir, "..", "tools", "bin", name))
	}
	if p, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "/opt/homebrew/bin/"+name, "/usr/local/bin/"+name, "/usr/bin/"+name)
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return name
}

// FindBRAW locates the Blackmagic RAW helper, or returns "" if it is not
// installed.
func FindBRAW() string {
	if p := FindTool("brawframes"); p != "brawframes" {
		return p
	}
	return ""
}
