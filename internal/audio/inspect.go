package audio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// probeTimeout bounds a single ffprobe invocation.
const probeTimeout = 60 * time.Second

// Source is the resolved audio source for a book: either one file with embedded
// chapter markers, or an ordered set of per-chapter files.
type Source struct {
	Style    string   // StyleMarkers | StyleFiles
	BookFile string   // StyleMarkers: the single audio file
	Files    []string // StyleFiles: audio files in name order
}

// ResolveSource maps a book's source path to its audio. A file path is the book
// file directly (marker style). A directory holding exactly one audio file is that
// file (marker style); a directory of several audio files is a multi-file book
// (one chapter per file, in name order).
func ResolveSource(sourcePath string) (Source, error) {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return Source{}, fmt.Errorf("stat source: %w", err)
	}
	if !info.IsDir() {
		if !IsAudio(sourcePath) {
			return Source{}, fmt.Errorf("source %q is not a recognized audio file", sourcePath)
		}
		return Source{Style: StyleMarkers, BookFile: sourcePath}, nil
	}
	files, err := audioFilesIn(sourcePath)
	if err != nil {
		return Source{}, err
	}
	switch len(files) {
	case 0:
		return Source{}, fmt.Errorf("no audio files in %q", sourcePath)
	case 1:
		return Source{Style: StyleMarkers, BookFile: files[0]}, nil
	default:
		return Source{Style: StyleFiles, Files: files}, nil
	}
}

// audioFilesIn lists the audio files directly in dir (non-recursive), sorted by
// name so multi-file chapter order is stable and matches the scanner.
func audioFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !IsAudio(e.Name()) {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)
	return files, nil
}

// Inspect probes a book's source audio, writes probe.json + (when a chapter list
// can be built) manifest.json into workDir, and reports whether the chapter
// markers are contiguous. A non-contiguous marker set writes no manifest and
// returns markersContiguous=false, deferring to the M5 markers_normalizing stage.
// A multi-file book is always contiguous (chapters are synthesized 1..N).
func Inspect(ctx context.Context, sourcePath, workDir, ffprobePath string) (Manifest, bool, error) {
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return Manifest{}, false, err
	}
	src, err := ResolveSource(sourcePath)
	if err != nil {
		return Manifest{}, false, err
	}
	if src.Style == StyleFiles {
		return inspectFiles(ctx, src.Files, workDir, ffprobePath)
	}
	return inspectMarkers(ctx, src.BookFile, workDir, ffprobePath)
}

// inspectMarkers probes a single chaptered file and builds a marker-derived
// manifest.
func inspectMarkers(ctx context.Context, bookFile, workDir, ffprobePath string) (Manifest, bool, error) {
	if ffprobePath == "" {
		return Manifest{}, false, errors.New("ffprobe unavailable; cannot read chapter markers")
	}
	raw, meta, err := probeFile(ctx, bookFile, ffprobePath, true)
	if err != nil {
		return Manifest{}, false, fmt.Errorf("ffprobe %s: %w", bookFile, err)
	}
	if err := writeFileAtomic(filepath.Join(workDir, ProbeName), raw); err != nil {
		return Manifest{}, false, err
	}

	var chapters []Chapter
	for _, ch := range meta.Chapters {
		num, title, ok := chapterFromMarker(ch.Tags["title"])
		if !ok {
			continue
		}
		start := parseFloat(ch.StartTime)
		end := parseFloat(ch.EndTime)
		chapters = append(chapters, Chapter{
			Chapter: num, Title: title, MarkerTitle: ch.Tags["title"],
			Start: start, End: end, Duration: end - start,
		})
	}
	sort.SliceStable(chapters, func(i, j int) bool { return chapters[i].Start < chapters[j].Start })

	if !contiguous(chapters) {
		// Logical markers need manual/agent mapping; do not guess a manifest.
		return Manifest{}, false, nil
	}
	m := Manifest{
		Source:       bookFile,
		Title:        meta.Format.Tags["title"],
		Style:        StyleMarkers,
		Duration:     parseFloat(meta.Format.Duration),
		ChapterCount: len(chapters),
		Chapters:     chapters,
	}
	if err := WriteManifest(workDir, m); err != nil {
		return Manifest{}, false, err
	}
	return m, true, nil
}

// inspectFiles builds a synthesized-chapter manifest for a multi-file book: one
// chapter per file in name order, with cumulative offsets from each file's probed
// duration (best-effort; a missing/failing ffprobe leaves durations 0).
func inspectFiles(ctx context.Context, files []string, workDir, ffprobePath string) (Manifest, bool, error) {
	var (
		chapters []Chapter
		offset   float64
		summary  = fileProbeSummary{Style: StyleFiles}
	)
	for i, f := range files {
		var dur float64
		if ffprobePath != "" {
			// A files-style book needs only each file's duration (format), not its
			// chapters - the chapters are synthesized one-per-file - so skip the
			// -show_chapters output.
			if _, meta, err := probeFile(ctx, f, ffprobePath, false); err == nil {
				dur = parseFloat(meta.Format.Duration)
			}
		}
		chapters = append(chapters, Chapter{
			Chapter: i + 1, MarkerTitle: filepath.Base(f),
			Start: offset, End: offset + dur, Duration: dur, FilePath: f,
		})
		summary.Files = append(summary.Files, fileProbeEntry{Path: f, Duration: dur})
		offset += dur
	}
	rawSummary, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return Manifest{}, false, err
	}
	if err := writeFileAtomic(filepath.Join(workDir, ProbeName), append(rawSummary, '\n')); err != nil {
		return Manifest{}, false, err
	}
	m := Manifest{
		Source:       filepath.Dir(files[0]),
		Style:        StyleFiles,
		Duration:     offset,
		ChapterCount: len(files),
		Chapters:     chapters,
	}
	if err := WriteManifest(workDir, m); err != nil {
		return Manifest{}, false, err
	}
	return m, true, nil
}

// fileProbeSummary is the probe.json written for a multi-file book (there is no
// single ffprobe document with embedded chapters, so record per-file durations).
type fileProbeSummary struct {
	Style string           `json:"style"`
	Files []fileProbeEntry `json:"files"`
}

type fileProbeEntry struct {
	Path     string  `json:"path"`
	Duration float64 `json:"duration"`
}

// probeMeta is the subset of ffprobe JSON inspect consumes.
type probeMeta struct {
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Chapters []struct {
		StartTime string            `json:"start_time"`
		EndTime   string            `json:"end_time"`
		Tags      map[string]string `json:"tags"`
	} `json:"chapters"`
}

// probeFile runs `ffprobe -show_format [-show_chapters] -of json` and returns both
// the raw output (for probe.json) and the parsed subset. showChapters is set only
// for a single-file marker book (which needs the embedded chapters); a files-style
// per-file probe wants just the duration, so it omits the chapter output.
func probeFile(ctx context.Context, path, ffprobePath string, showChapters bool) ([]byte, probeMeta, error) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	args := []string{"-v", "error", "-show_format"}
	if showChapters {
		args = append(args, "-show_chapters")
	}
	args = append(args, "-of", "json", path)
	//nolint:gosec // ffprobePath is an operator-resolved tool path; path is a library file
	cmd := exec.CommandContext(cctx, ffprobePath, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, probeMeta{}, err
	}
	var meta probeMeta
	if err := json.Unmarshal(out, &meta); err != nil {
		return nil, probeMeta{}, fmt.Errorf("parse ffprobe json: %w", err)
	}
	return out, meta, nil
}

// parseFloat parses an ffprobe numeric string, yielding 0 on failure.
func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
