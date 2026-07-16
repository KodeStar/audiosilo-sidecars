// Package audio implements the mechanical audio stages of the extraction
// pipeline: inspect (ffprobe the source, normalize chapter markers into a
// manifest) and split (ffmpeg each chapter into a mono/16 kHz FLAC ready for
// ASR). It is a faithful port of the historical audio_extract.py, generalized to
// also handle multi-file books, and kept free of any scheduler/store concerns so
// the logic stays unit-testable.
//
// Two artifacts land in the book's work dir:
//
//   - probe.json  - the raw ffprobe output (format + chapters) for the record.
//   - manifest.json - the normalized chapter list the split (and later ASR)
//     stages consume: one entry per logical chapter with start/end/duration and,
//     for a multi-file book, the source file. Its shape matches the historical
//     manifest so past work dirs stay readable.
package audio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Artifact filenames inside a book's work dir.
const (
	ManifestName = "manifest.json"
	ProbeName    = "probe.json"
	ChaptersDir  = "chapters"
)

// Style classifies how a book's chapters map onto audio files.
const (
	// StyleMarkers: one audio file whose embedded chapter markers define the
	// chapters (the dominant single-.m4b case). Split seeks within the file.
	StyleMarkers = "markers"
	// StyleFiles: multiple audio files, one chapter per file in name order. Split
	// converts each whole file.
	StyleFiles = "files"
)

// Chapter is one normalized chapter on the whole-book timeline. Start/End/Duration
// are seconds; for a StyleFiles book FilePath names the source audio file.
type Chapter struct {
	Chapter     int     `json:"chapter"`
	Title       string  `json:"title,omitempty"`
	MarkerTitle string  `json:"marker_title,omitempty"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Duration    float64 `json:"duration"`
	FilePath    string  `json:"file_path,omitempty"`
}

// Manifest is the normalized inspect output: the audio source, its title/duration,
// the chapter split style, and the ordered chapters.
type Manifest struct {
	Source       string    `json:"source"`
	Title        string    `json:"title,omitempty"`
	Style        string    `json:"style"`
	Duration     float64   `json:"duration"`
	ChapterCount int       `json:"chapter_count"`
	Chapters     []Chapter `json:"chapters"`
}

// audioExts mirrors audiosilo-meta pkg/scan's recognized audiobook extensions, so
// a folder the scanner accepted resolves the same audio files here.
var audioExts = map[string]bool{
	".m4b": true, ".m4a": true, ".mp4": true, ".mp3": true, ".aac": true,
	".ogg": true, ".opus": true, ".flac": true, ".wma": true,
}

// IsAudio reports whether name has a recognized audiobook extension.
func IsAudio(name string) bool {
	return audioExts[strings.ToLower(filepath.Ext(name))]
}

// Marker regexes ported verbatim from audio_extract.py's chapter_from_marker: the
// marker style varies per book, so accept "Chapter N", "Chapter N: Title",
// "Chapter N. Title", "Chapter N - Title" (hyphen), and the bare "N. Title" form.
// Credits ("Opening Credits" / "End Credits") match none and are excluded.
var (
	reChapterMarker = regexp.MustCompile(`(?i)^Chapter\s+(\d+)(?:\s*[.:-]\s*(.*))?$`)
	reNumberDot     = regexp.MustCompile(`^(\d+)\.\s*(.*)$`)
)

// chapterFromMarker parses a chapter number and optional title from a marker
// title, or ok=false when it is not a chapter marker.
func chapterFromMarker(title string) (num int, chapterTitle string, ok bool) {
	t := strings.TrimSpace(title)
	if m := reChapterMarker.FindStringSubmatch(t); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, "", false
		}
		return n, strings.TrimSpace(m[2]), true
	}
	if m := reNumberDot.FindStringSubmatch(t); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, "", false
		}
		return n, strings.TrimSpace(m[2]), true
	}
	return 0, "", false
}

// contiguous reports whether chapters (already sorted by start) number a gapless
// run i, i+1, ... starting at 0 or 1 - the historical validation. An empty list is
// not contiguous. When false, the markers need the M5 markers_normalizing agent
// stage, so inspect leaves markersContiguous false rather than guessing a mapping.
func contiguous(chs []Chapter) bool {
	if len(chs) == 0 {
		return false
	}
	first := chs[0].Chapter
	if first != 0 && first != 1 {
		return false
	}
	for i, ch := range chs {
		if ch.Chapter != first+i {
			return false
		}
	}
	return true
}

// WriteManifest writes m to workDir/manifest.json atomically (temp + rename).
func WriteManifest(workDir string, m Manifest) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(workDir, ManifestName), append(out, '\n'))
}

// ReadManifest loads workDir/manifest.json.
func ReadManifest(workDir string) (Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, ManifestName)) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// writeFileAtomic writes data to path via a sibling temp file + rename, so a
// crash never leaves a half-written artifact a later run would trust.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // non-secret artifact
		return err
	}
	return os.Rename(tmp, path)
}
