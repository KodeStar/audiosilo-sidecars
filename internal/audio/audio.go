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

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
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

var chapterNumberWords = map[string]int{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4,
	"five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9,
	"ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13,
	"fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17,
	"eighteen": 18, "nineteen": 19, "twenty": 20, "thirty": 30,
	"forty": 40, "fifty": 50, "sixty": 60, "seventy": 70,
	"eighty": 80, "ninety": 90,
}

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
	// Some M4B encoders spell marker numbers out ("Chapter Twenty One") rather
	// than using digits. Treat those exactly like their numeric equivalents so a
	// complete embedded chapter table does not get discarded and sent through
	// agent-based marker recovery. A colon, dot, or spaced hyphen separates an
	// optional chapter title; a hyphen inside "twenty-one" remains part of the
	// number phrase.
	if len(t) > len("chapter ") && strings.EqualFold(t[:len("chapter ")], "chapter ") {
		rest := strings.TrimSpace(t[len("chapter "):])
		words, suffix := splitWordChapterTitle(rest)
		if n, valid := parseChapterNumberWords(words); valid {
			return n, suffix, true
		}
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

func splitWordChapterTitle(s string) (words, title string) {
	cut := len(s)
	delimLen := 0
	for _, delim := range []string{" - ", " – ", " — ", ":", "."} {
		if i := strings.Index(s, delim); i >= 0 && i < cut {
			cut, delimLen = i, len(delim)
		}
	}
	if cut == len(s) {
		return strings.TrimSpace(s), ""
	}
	return strings.TrimSpace(s[:cut]), strings.TrimSpace(s[cut+delimLen:])
}

// parseChapterNumberWords parses the conventional English cardinal form used by
// audiobook chapter markers. It deliberately rejects unknown words rather than
// guessing, while supporting hyphenated numbers and chapters above one hundred.
func parseChapterNumberWords(s string) (int, bool) {
	fields := strings.Fields(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", " "))
	if len(fields) == 0 {
		return 0, false
	}
	total, current := 0, 0
	seen := false
	for _, word := range fields {
		if word == "and" {
			continue
		}
		if value, ok := chapterNumberWords[word]; ok {
			current += value
			seen = true
			continue
		}
		switch word {
		case "hundred":
			if current == 0 {
				current = 1
			}
			current *= 100
			seen = true
		case "thousand":
			if current == 0 {
				current = 1
			}
			total += current * 1000
			current = 0
			seen = true
		default:
			return 0, false
		}
	}
	return total + current, seen
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

// Contiguous reports whether chs number a gapless run starting at 0 or 1 (the
// historical validation), exported so the markers_normalizing agent stage can
// validate an agent-produced manifest against the same rule inspect used. It
// delegates to the unexported contiguous so there is a single implementation.
func Contiguous(chs []Chapter) bool { return contiguous(chs) }

// WriteManifest writes m to workDir/manifest.json atomically (temp + rename).
func WriteManifest(workDir string, m Manifest) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, ManifestName), append(out, '\n'), 0o644)
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
