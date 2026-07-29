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
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// MarkerStats reports what the marker parser SAW versus what it understood, so a
// caller can tell the two very different empty-manifest causes apart:
//
//   - Seen == 0: the file carries no embedded chapter markers at all. Nothing to
//     parse; only a human (or a per-file split) can chapter this book.
//   - Seen > 0 && Recognized == 0: the file carries a full marker table written in a
//     dialect chapterFromMarker does not know. That is a one-line vocabulary gap with
//     a free deterministic recovery (ReparseMarkerManifest after a parser upgrade),
//     NOT an unfixable book - but it used to look identical to the case above in the
//     logs, so each new dialect had to be rediscovered from a parked book.
//
// A StyleFiles book has no marker table to parse at all (its chapters are synthesized
// one per file, and its probe.json is a per-file duration summary), so both counts stay
// 0 and NoneRecognized is false - reporting its file count as markers would fabricate
// the very signal these stats exist to make trustworthy.
type MarkerStats struct {
	Seen       int  // embedded markers present in probe.json (0 for a StyleFiles book)
	Recognized int  // markers chapterFromMarker understood (== Manifest.ChapterCount for StyleMarkers)
	Contiguous bool // the recognized chapters form a gapless 0/1-based run
	// Positional records that the whole table was numbered by its time order because no
	// marker stated a number (see positionalChapters). It is reported so the choice is
	// visible in the stage metrics rather than silently indistinguishable from a table
	// whose titles carried numbers all along.
	Positional bool
	// Unmapped is every run of recording time no chapter covers, with the marker titles
	// that occupy it. Complete is the verdict over it (see Complete).
	Unmapped []UnmappedSpan
	Complete bool
}

// NoneRecognized reports the vocabulary-gap condition: markers were present and the
// parser understood none of them.
func (s MarkerStats) NoneRecognized() bool { return s.Seen > 0 && s.Recognized == 0 }

// Usable is the ROUTING verdict: the chapter map both numbers cleanly AND loses no
// narration. Both halves are load-bearing and neither implies the other - a book whose
// unnumbered interludes were dropped still numbers 1..N perfectly, which is exactly how
// 27 books lost 61 hours of narration with no park, no note and no agent round.
func (s MarkerStats) Usable() bool { return s.Contiguous && s.Complete }

// Marker is one raw embedded chapter marker as ffprobe reports it, before any attempt
// to read a chapter number out of its title. Carrying the raw table alongside the parsed
// chapters is what lets a caller see the markers the parser DROPPED.
type Marker struct {
	Title string
	Start float64
	End   float64
}

// Span positions. A leading/trailing run of unmapped audio is routinely legitimate
// (opening and closing credits are not chapters); an interior one never is.
const (
	SpanLeading  = "leading"
	SpanInterior = "interior"
	SpanTrailing = "trailing"
)

// MaxInteriorGapSec bounds how much recording time may sit BETWEEN two mapped chapters
// before the map is treated as having lost narration.
//
// 60s sits in an empty band measured over the whole 294-book corpus: interior holes are
// sharply bimodal, one hole of 9s (an encoder artifact) and then NOTHING until 211s,
// above which all 173 of them are real narration (Interlude / Side Story / Intermission /
// Stat Sheet / a split "Chapter 10a"). So the threshold has a 6x margin below the
// smallest real hole and a 6x margin above the largest artifact.
const MaxInteriorGapSec = 60.0

// MaxEdgeGapSec bounds a LEADING or TRAILING run of unmapped audio. It is deliberately
// looser than the interior bound because the edges are where non-chapter material
// legitimately lives (credits, dedications, retailer samples, a preview of the next
// book, bloopers). It matches the pipeline's existing "short enough to be non-narrative"
// definition, so an edge run long enough to hold a Prologue or an Epilogue - the two
// real-content losses the corpus showed at the edges, up to 45 minutes of it - is
// referred for a mapping decision instead of being dropped on the parser's silence.
const MaxEdgeGapSec = 180.0

// UnmappedSpan is a run of recording time that no manifest chapter covers, with the raw
// marker titles occupying it and where it sits relative to the mapped chapters.
type UnmappedSpan struct {
	Start    float64  `json:"start"`
	End      float64  `json:"end"`
	Position string   `json:"position"`
	Titles   []string `json:"titles,omitempty"`
}

// Duration is the span's length in seconds.
func (s UnmappedSpan) Duration() float64 { return s.End - s.Start }

// Tolerated reports whether the span is short enough to be ordinary non-chapter
// material at its position.
func (s UnmappedSpan) Tolerated() bool {
	if s.Position == SpanInterior {
		return s.Duration() < MaxInteriorGapSec
	}
	return s.Duration() < MaxEdgeGapSec
}

// minReportedSpanSec keeps sub-second float noise between adjacent chapter intervals out
// of the reported spans.
const minReportedSpanSec = 1.0

// UnmappedSpans returns every run of [0, duration] that no chapter in chs covers, in
// time order, annotated with the raw markers that occupy it. An empty chapter list
// yields the whole recording as one interior span (nothing is mapped, so nothing is
// "leading" or "trailing" anything).
func UnmappedSpans(chs []Chapter, markers []Marker, duration float64) []UnmappedSpan {
	if duration <= 0 {
		return nil
	}
	covered := mergedCover(chs)
	if len(covered) == 0 {
		return []UnmappedSpan{withTitles(UnmappedSpan{Start: 0, End: duration, Position: SpanInterior}, markers)}
	}
	var spans []UnmappedSpan
	add := func(start, end float64, pos string) {
		if end-start >= minReportedSpanSec {
			spans = append(spans, withTitles(UnmappedSpan{Start: start, End: end, Position: pos}, markers))
		}
	}
	add(0, covered[0][0], SpanLeading)
	for i := 0; i+1 < len(covered); i++ {
		add(covered[i][1], covered[i+1][0], SpanInterior)
	}
	add(covered[len(covered)-1][1], duration, SpanTrailing)
	return spans
}

// mergedCover returns chs' intervals sorted and merged, so an unmapped run is a genuine
// hole rather than an artifact of overlapping or out-of-order chapters.
func mergedCover(chs []Chapter) [][2]float64 {
	ivals := make([][2]float64, 0, len(chs))
	for _, ch := range chs {
		if ch.End > ch.Start {
			ivals = append(ivals, [2]float64{ch.Start, ch.End})
		}
	}
	sort.Slice(ivals, func(i, j int) bool { return ivals[i][0] < ivals[j][0] })
	var out [][2]float64
	for _, iv := range ivals {
		if n := len(out); n > 0 && iv[0] <= out[n-1][1] {
			if iv[1] > out[n-1][1] {
				out[n-1][1] = iv[1]
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// withTitles attaches the markers that sit MOSTLY inside the span (over half their own
// length), so a marker straddling a chapter boundary is named once, against the span
// that actually holds it.
func withTitles(s UnmappedSpan, markers []Marker) UnmappedSpan {
	for _, mk := range markers {
		overlap := math.Min(s.End, mk.End) - math.Max(s.Start, mk.Start)
		if overlap <= 0 {
			continue
		}
		if length := mk.End - mk.Start; length <= 0 || overlap*2 >= length {
			if t := strings.TrimSpace(mk.Title); t != "" {
				s.Titles = append(s.Titles, t)
			}
		}
	}
	return s
}

// Complete reports whether every unmapped span is short enough to be ordinary
// non-chapter material at its position - that is, whether the chapter map loses no
// narration.
func Complete(spans []UnmappedSpan) bool {
	for _, s := range spans {
		if !s.Tolerated() {
			return false
		}
	}
	return true
}

// DescribeSpans renders the intolerable spans for a log line or an agent prompt:
// "3.9 min at 36266-38276s (interior; Mini Stories)".
func DescribeSpans(spans []UnmappedSpan) string {
	var parts []string
	for _, s := range spans {
		if s.Tolerated() {
			continue
		}
		part := fmt.Sprintf("%.1f min at %.0f-%.0fs (%s", s.Duration()/60, s.Start, s.End, s.Position)
		if len(s.Titles) > 0 {
			part += "; " + strings.Join(s.Titles, ", ")
		}
		parts = append(parts, part+")")
	}
	return strings.Join(parts, "; ")
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
// "Chapter N. Title", "Chapter N - Title" (hyphen), and a leading number with an
// optional ". Title" tail ("1. Troll Hunt" or a bare "001"). Credits ("Opening
// Credits" / "End Credits") match none and are excluded.
var (
	// Some Audible-style M4Bs use "Chapter: 1 – Title": a separator appears
	// between the word Chapter and its number, and a Unicode dash separates the
	// title. Accept that alongside the older "Chapter 1: Title" forms.
	//
	// The title may also be introduced by NOTHING but whitespace ("Chapter 1 Suffering
	// from Success", "Chapter 1 (Eternity's Battleship)") or by an underscore
	// ("Chapter_1"); five real books' whole tables were discarded for want of a
	// punctuation separator. Unlike the bare-number form below, requiring the literal
	// word "Chapter" first removes the ambiguity that makes a loose tail dangerous
	// there: "Chapter 1 Some Title" can only be announcing chapter 1.
	//
	// The tail must still be introduced by a separator character, which is what keeps
	// "Chapter 10a" REJECTED: the digits cannot be followed straight by a letter. That
	// exclusion is deliberate - a split chapter announces the same number twice, which
	// no contiguous single-number manifest can express, so those books belong with the
	// mapping agent.
	reChapterMarker = regexp.MustCompile(`(?i)^Chapter(?:\s*[:_]\s*|\s+)(\d+)(?:[\s.:\-–—_]+(.*))?$`)
	// A bare number wrapped in dashes ("-1-".."-40-"), another titleless full sequence.
	reDashNumberMarker = regexp.MustCompile(`^-\s*(\d+)\s*-$`)
	// The ". Title" tail is OPTIONAL because many M4Bs label markers with nothing but
	// a zero-padded track number ("001".."064") - titleless, but a fully explicit
	// sequence. Requiring the dot discarded every such marker, leaving an EMPTY draft
	// manifest that markers_normalizing could only park on. A bare number is trusted
	// no more than "N." already was: contiguous() still routes a gappy set to the
	// agent, and a numbered credits marker is dropped downstream by the
	// content-driven classifyBookEdges, never here. Do NOT reduce the tail to
	// `\.?\s*(.*)` - that also swallows "1a" and "1 Some Title" as chapter 1.
	reNumberMarker = regexp.MustCompile(`^(\d+)(?:\.\s*(.*))?$`)
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
	// A leading number, with or without a ". Title" tail. Atoi still guards the
	// range: the pattern proves the digits, not that they fit an int.
	if m := reNumberMarker.FindStringSubmatch(t); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, "", false
		}
		return n, strings.TrimSpace(m[2]), true
	}
	if m := reDashNumberMarker.FindStringSubmatch(t); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, "", false
		}
		return n, "", true
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

// spokenAnnouncementWords bounds how far into a chapter's narration an opening chapter
// announcement may run. A narrator says "One.", "18.", "Chapter Twenty One." and then
// begins the prose; front matter says "Prologue" / "Epilogue" and runs straight on with no
// sentence break for a long while. Capping the candidate at a few words means an unnumbered
// section is rejected on its own opening rather than by scanning to a distant period.
const spokenAnnouncementWords = 4

// SpokenChapterNumber parses the chapter number a narrator ANNOUNCES at the very start of a
// chapter's transcript ("One. Four more deaths..." -> 1, "18. Havoc paused..." -> 18,
// "Fifty-nine. Thanks for the save..." -> 59), or ok=false when the opening is not a chapter
// announcement at all ("Prologue Hello...", "Epilogue Getting out...", "Bloopers! He...").
//
// It exists because an audio file's POSITION is not its chapter number: a book with an
// unnumbered Prologue in file 2 has chapter 1 in file 3. The number the narrator speaks is
// the only direct evidence of that mapping, and it is the numbering the meta schema's
// position.chapter refers to.
//
// The whole candidate must parse as one number: the announcement is taken up to the first
// sentence terminator (capped at spokenAnnouncementWords), an optional leading "chapter" is
// dropped, and the remainder is parsed by the same vocabulary as the marker parser -
// which rejects unknown words rather than guessing. That total-consumption rule is what
// keeps prose out: a chapter opening "One more time, Joe..." yields the candidate
// "one more time" and is rejected, rather than reading as chapter 1.
func SpokenChapterNumber(text string) (int, bool) {
	candidate := strings.TrimSpace(text)
	if i := strings.IndexAny(candidate, ".!?"); i >= 0 {
		candidate = candidate[:i]
	}
	fields := strings.Fields(candidate)
	if len(fields) == 0 || len(fields) > spokenAnnouncementWords {
		return 0, false
	}
	if strings.EqualFold(fields[0], "chapter") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return 0, false
	}
	joined := strings.Join(fields, " ")
	if n, err := strconv.Atoi(joined); err == nil {
		return n, n >= 0
	}
	return parseChapterNumberWords(joined)
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
