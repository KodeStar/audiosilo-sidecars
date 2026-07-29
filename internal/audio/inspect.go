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
	"strings"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/scan"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
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

// audioFilesIn lists the audio files directly in dir (non-recursive), in
// numeric-aware ("natural") name order so multi-file chapter order is stable and
// intuitive: "Chapter 2" sorts before "Chapter 10", which a plain byte sort gets
// wrong for unpadded names. The order IS audiosilo-meta pkg/scan's exported
// comparator (scan.NaturalLess - the same import, not a copy), so the split
// order here is BY CONSTRUCTION how the shared scanner enumerates the same
// folder. That matters because this enumeration determines chapter numbers,
// which spoiler-gate community sidecars (position.chapter) - a divergent local
// copy could silently misalign spoiler positions.
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
	sort.SliceStable(files, func(i, j int) bool {
		return scan.NaturalLess(filepath.Base(files[i]), filepath.Base(files[j]))
	})
	return files, nil
}

// Inspect probes a book's source audio, writes probe.json + a manifest.json into
// workDir, and reports what the marker parser saw (MarkerStats, whose Contiguous
// drives routing). A non-contiguous marker set still writes manifest.json - as a
// DRAFT the M5 markers_normalizing stage corrects - but returns Contiguous=false so
// the state machine routes there first. A multi-file book is always contiguous
// (chapters are synthesized 1..N).
func Inspect(ctx context.Context, sourcePath, workDir, ffprobePath string) (Manifest, MarkerStats, error) {
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return Manifest{}, MarkerStats{}, err
	}
	src, err := ResolveSource(sourcePath)
	if err != nil {
		return Manifest{}, MarkerStats{}, err
	}
	if src.Style == StyleFiles {
		return inspectFiles(ctx, src.Files, workDir, ffprobePath)
	}
	return inspectMarkers(ctx, src.BookFile, workDir, ffprobePath)
}

// inspectMarkers probes a single chaptered file and builds a marker-derived
// manifest.
func inspectMarkers(ctx context.Context, bookFile, workDir, ffprobePath string) (Manifest, MarkerStats, error) {
	if ffprobePath == "" {
		return Manifest{}, MarkerStats{}, errors.New("ffprobe unavailable; cannot read chapter markers")
	}
	raw, meta, err := probeFile(ctx, bookFile, ffprobePath, true)
	if err != nil {
		return Manifest{}, MarkerStats{}, fmt.Errorf("ffprobe %s: %w", bookFile, err)
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(workDir, ProbeName), raw, 0o644); err != nil {
		return Manifest{}, MarkerStats{}, err
	}

	m, stats := markerManifestFromProbe(bookFile, meta)
	if err := WriteManifest(workDir, m); err != nil {
		return Manifest{}, MarkerStats{}, err
	}
	return m, stats, nil
}

// ReparseMarkerManifest rebuilds an already-inspected marker manifest from its
// durable probe.json using the current marker parser. It is the upgrade recovery
// path for books inspected by an older parser that discarded a now-supported
// marker style: marker normalization can recover deterministically without another
// ffprobe call or an agent guessing a manifest schema.
func ReparseMarkerManifest(workDir string, draft Manifest) (Manifest, MarkerStats, error) {
	if draft.Style != StyleMarkers {
		return Manifest{}, MarkerStats{}, fmt.Errorf("cannot reparse non-marker manifest style %q", draft.Style)
	}
	raw, err := os.ReadFile(filepath.Join(workDir, ProbeName)) //nolint:gosec // workDir is the book's managed scratch directory
	if err != nil {
		return Manifest{}, MarkerStats{}, fmt.Errorf("read %s: %w", ProbeName, err)
	}
	var meta probeMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Manifest{}, MarkerStats{}, fmt.Errorf("parse %s: %w", ProbeName, err)
	}
	m, stats := markerManifestFromProbe(draft.Source, meta)
	if draft.Title != "" {
		m.Title = draft.Title
	}
	// The probe is authoritative for duration when it states one, but a probe.json
	// without a parseable format.duration must not ZERO a duration the draft already
	// knew. markers_normalizing bounds the agent's corrected intervals against this
	// value, so a zeroed duration rejects every correct mapping the agent can produce.
	if m.Duration == 0 {
		m.Duration = draft.Duration
	}
	if err := WriteManifest(workDir, m); err != nil {
		return Manifest{}, MarkerStats{}, err
	}
	return m, stats, nil
}

// ReadProbeMarkers loads the raw marker table and recording duration from an already
// written probe.json. It is how a caller downstream of inspect (the marker-normalization
// validator) can ask what the recording ACTUALLY contains, rather than trusting a manifest
// that may have dropped markers.
func ReadProbeMarkers(workDir string) ([]Marker, float64, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, ProbeName)) //nolint:gosec // workDir is the book's managed scratch directory
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", ProbeName, err)
	}
	var meta probeMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, 0, fmt.Errorf("parse %s: %w", ProbeName, err)
	}
	markers := make([]Marker, 0, len(meta.Chapters))
	for _, ch := range meta.Chapters {
		markers = append(markers, Marker{
			Title: ch.Tags["title"],
			Start: parseFloat(ch.StartTime),
			End:   parseFloat(ch.EndTime),
		})
	}
	sort.SliceStable(markers, func(i, j int) bool { return markers[i].Start < markers[j].Start })
	return markers, parseFloat(meta.Format.Duration), nil
}

func markerManifestFromProbe(source string, meta probeMeta) (Manifest, MarkerStats) {
	markers := make([]Marker, 0, len(meta.Chapters))
	for _, ch := range meta.Chapters {
		markers = append(markers, Marker{
			Title: ch.Tags["title"],
			Start: parseFloat(ch.StartTime),
			End:   parseFloat(ch.EndTime),
		})
	}
	sort.SliceStable(markers, func(i, j int) bool { return markers[i].Start < markers[j].Start })

	var chapters []Chapter
	for _, mk := range markers {
		num, title, ok := chapterFromMarker(mk.Title)
		if !ok {
			continue
		}
		chapters = append(chapters, Chapter{
			Chapter: num, Title: title, MarkerTitle: mk.Title,
			Start: mk.Start, End: mk.End, Duration: mk.End - mk.Start,
		})
	}
	sort.SliceStable(chapters, func(i, j int) bool { return chapters[i].Start < chapters[j].Start })

	positional := false
	if len(chapters) == 0 {
		if tiled := positionalChapters(markers); tiled != nil {
			chapters, positional = tiled, true
		}
	}

	duration := parseFloat(meta.Format.Duration)
	spans := UnmappedSpans(chapters, markers, duration)

	// Always write the manifest, even for an unusable marker set: it is the DRAFT the
	// markers_normalizing agent stage reads and corrects (renumber/exclude/retitle).
	// MarkerStats.Usable() is still the routing decision - when it is false the state
	// machine sends the book to markers_normalizing before split, which never runs on a
	// draft.
	m := Manifest{
		Source:       source,
		Title:        meta.Format.Tags["title"],
		Style:        StyleMarkers,
		Duration:     duration,
		ChapterCount: len(chapters),
		Chapters:     chapters,
	}
	// Seen counts the RAW markers, so a table written in an unknown dialect (every
	// marker dropped above) stays distinguishable from a genuinely markerless file.
	// Recognized stays the honest PARSER count, so a positionally-numbered table still
	// reports Recognized 0 - Positional is what says the chapters came from time order.
	stats := MarkerStats{
		Seen:       len(markers),
		Recognized: len(chapters),
		Contiguous: contiguous(chapters),
		Positional: positional,
		Unmapped:   spans,
		Complete:   Complete(spans),
	}
	if positional {
		stats.Recognized = 0
	}
	return m, stats
}

// positionalChapters numbers a marker table 1..N by its TIME order, for the one case
// where that is the only defensible reading: no marker states a number at all, and the
// markers tile the narration without a hole.
//
// A publisher who labels chapters with their titles ("Transfer Paperwork", "On the
// Nature of Shadows") has still stated the order - in the table's own sequence - and a
// multi-file book of exactly the same content is already numbered this way by file
// order, with no agent asked. Refusing here only for a single-file book was an
// inconsistency, not a safety property: 12 of the corpus's 13 title-only books were sent
// to the agent, which produced this very mapping, and the 13th was declined and parked.
//
// It returns nil unless the table tiles, because a gappy title-only table gives no
// evidence that the markers are the chapters, and a hole is exactly what must reach a
// human. Credits at the ends are numbered like any other marker and dropped downstream by
// the content-driven classifyBookEdges, the same treatment a bare-number "001".."064"
// table already gets.
func positionalChapters(markers []Marker) []Chapter {
	if len(markers) == 0 {
		return nil
	}
	for i, mk := range markers {
		if mk.End <= mk.Start {
			return nil
		}
		if i > 0 && mk.Start-markers[i-1].End >= MaxInteriorGapSec {
			return nil
		}
	}
	chs := make([]Chapter, 0, len(markers))
	for i, mk := range markers {
		chs = append(chs, Chapter{
			Chapter: i + 1, Title: strings.TrimSpace(mk.Title), MarkerTitle: mk.Title,
			Start: mk.Start, End: mk.End, Duration: mk.End - mk.Start,
		})
	}
	return chs
}

// inspectFiles builds a synthesized-chapter manifest for a multi-file book: one
// chapter per file in name order, with cumulative offsets from each file's probed
// duration (best-effort; a missing/failing ffprobe leaves durations 0).
func inspectFiles(ctx context.Context, files []string, workDir, ffprobePath string) (Manifest, MarkerStats, error) {
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
		return Manifest{}, MarkerStats{}, err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(workDir, ProbeName), append(rawSummary, '\n'), 0o644); err != nil {
		return Manifest{}, MarkerStats{}, err
	}
	m := Manifest{
		Source:       filepath.Dir(files[0]),
		Style:        StyleFiles,
		Duration:     offset,
		ChapterCount: len(files),
		Chapters:     chapters,
	}
	if err := WriteManifest(workDir, m); err != nil {
		return Manifest{}, MarkerStats{}, err
	}
	// A files-style book parses no markers - its chapters ARE its files, and its
	// probe.json is a per-file duration summary with no marker table at all. Report
	// Seen/Recognized as 0 rather than the file count: the whole point of Seen is to make
	// an unread marker dialect visible in the metrics, so reporting a marker count for a
	// book that has none would poison exactly that signal. NoneRecognized() stays false
	// (it needs Seen > 0), which is the correct verdict here. Its chapters are laid end to
	// end from each file's own duration, so the map covers the book by construction.
	return m, MarkerStats{Contiguous: true, Complete: true}, nil
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
