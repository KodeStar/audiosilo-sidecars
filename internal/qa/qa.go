// Package qa implements the mechanical transcript-QA "degeneration sweep" of
// EXTRACTION-AUDIO.md: the detectors that flag where an ASR pass has degenerated
// (Whisper looping a phrase, hallucinating a tail over silence, or dropping a
// chapter's word rate off a cliff) so a chapter can be re-transcribed or handed to
// a human before any facts are drawn from it. It is a faithful Go port of a family
// of Python scripts hand-run over 11+ books; the thresholds and edge cases here ARE
// the contract those books validated, so they are preserved exactly (including the
// per-detector asymmetries in chapter-0 handling and truthiness noted below).
//
// The detectors, and the historical defect each was built to catch:
//
//   - WPH outliers (qa_sweep.py): words-per-hour more than 2.5 sample standard
//     deviations from the book mean. A chapter that transcribed far too fast or
//     slow is suspect. Excludes chapter 0 (a publisher "story so far" recap that
//     sits outside the position model).
//   - Repeated-segment runs (qa_sweep.py): >= 3 consecutive segments with identical
//     normalized text. Split into a benign chapter-END fade (run starts at >= 85% of
//     the chapter) and a dangerous MID-CHAPTER loop (it overwrites real narration).
//     Excludes chapter 0.
//   - Low-confidence words (qa_sweep.py): the rate of words the model scored below
//     0.5. Informational only - it never affects the clean verdict or the queue.
//   - Cross-segment 6-gram loops (cross_segment_scan.py): a 6-gram repeated >= 5x
//     across a whole chapter. This is the THIRD degeneration class, found by the
//     HW03 ch044 fact-pass agent and missed by the two detectors above: the looping
//     segments are only NEAR-identical (so the identical-run detector sees no two
//     equal), the loop spans many segments with only 1-2 repeats inside any one (so
//     the within-segment detector is under threshold), and it REPLACES narration
//     rather than adding to it (so the word rate stays normal). Includes chapter 0.
//   - Within-segment 6-gram loops (within_segment_scan.py): a 6-gram repeated >= 8x
//     inside ONE segment. Excludes chapter 0.
//   - Multi-loop scan (multi_loop_scan.py): EVERY 6-gram at or above 5 repeats in a
//     chapter, not just the worst one, deduped by the set of its words. Built after
//     HW04 ch012 hid a 28-second mid-chapter loop behind a smaller tail echo (the
//     cross-segment detector reports only the single most common gram, so the second
//     loop was invisible) and after a repair surfaced a loop the first was masking.
//     Prefers the transcripts-repaired/ layer where present. Includes chapter 0.
//   - Tail rate (tail_rate_scan.py): the final 12 words spoken faster than 4.5
//     words/second - a physical impossibility for real narration. This is the
//     threshold-free backstop for HW04: every repeat-count detector has a floor the
//     book kept sliding under, and no 6-gram detector can EVER catch a one-word loop
//     (ch056's "do do do do" cannot repeat a 6-gram at all). Whisper crams the words
//     it invents over silence into the slivers of time that remain, so measuring the
//     physics catches appended tails, short-word loops and crammed fragments alike.
//     Includes chapter 0.
//
// Input is the normalized transcript layer (transcripts-json/chNNN.json,
// audiosilo-transcript/v1) that the sanitizing stage already wrote - NOT the
// immutable raw layer. The Python scripts read the raw openai-whisper JSON, whose
// top-level "text" is the plain concatenation of the segment texts and whose
// per-word fields map 1:1 onto our normalized Word, so reading the normalized layer
// is semantically identical (see the note on chapter text below). This package
// never writes into any transcript layer; it produces qa_report.json / qa_report.md
// in the work dir.
//
// Chapter durations are supplied by the caller (the pipeline reads them from
// manifest.json via internal/audio) so this package need not depend on internal/audio.
package qa

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// Report file names written into the work dir.
const (
	ReportJSONName = "qa_report.json"
	ReportMDName   = "qa_report.md"
)

// Loop kinds for a repeated-segment run: the stable qa_report.json enum values.
// The historical display labels ("end-fade" / "MID-CHAPTER LOOP") are a rendering
// concern - kindLabel in report.go maps to them for the markdown - so the wire
// contract can stay a clean enum without breaking golden parity with qa_sweep.py.
const (
	KindEndFade    = "end_fade"
	KindMidChapter = "mid_chapter"
)

// Detector thresholds and constants - ported verbatim from the Python scripts. Do
// not tune these without re-validating against the historical books; they are the
// contract, not a default.
const (
	wphZThreshold   = 2.5  // qa_sweep.py: flag |z| > 2.5
	endFadePosition = 0.85 // qa_sweep.py: a run starting at >= 85% of the chapter is a benign end fade
	repeatRunMin    = 3    // qa_sweep.py: >= 3 identical consecutive segments is a run

	gramSize = 6 // every 6-gram detector uses a window of 6 tokens

	crossMinTokens = 50 // cross_segment_scan.py: skip a chapter with < 50 tokens
	crossThreshold = 5  // cross_segment_scan.py: a 6-gram repeated >= 5x is not natural prose

	withinMinTokens = 12 // within_segment_scan.py: skip a segment with < 12 tokens
	withinThreshold = 8  // within_segment_scan.py: a 6-gram repeated >= 8x inside one segment

	multiMinTokens = 50   // multi_loop_scan.py: skip a chapter with < 50 tokens
	multiThreshold = 5    // multi_loop_scan.py: report every 6-gram repeated >= 5x
	multiMidMax    = 85.0 // multi_loop_scan.py: 0 <= pos% < 85 is a (dangerous) mid-chapter loop

	tailWords     = 12    // tail_rate_scan.py: measure the final 12 words
	tailMaxWPS    = 4.5   // tail_rate_scan.py: ~1.8x normal narration; real speech never sustains this
	tailSpanFloor = 0.001 // tail_rate_scan.py: floor the span so a zero-length tail cannot divide by zero
	tailSkipSlack = 5     // tail_rate_scan.py: skip a chapter with fewer than tailWords + 5 words

	lowConfProb   = 0.5 // qa_sweep.py: a word scored below 0.5 is low-confidence
	lowConfWorstN = 5   // qa_sweep.py: report the 5 worst chapters by low-confidence rate

	snippetLen  = 60 // qa_sweep.py: repeated-run snippet truncated to 60 chars
	multiPhrase = 46 // multi_loop_scan.py: phrase truncated to 46 chars in its line
	tailTextLen = 52 // tail_rate_scan.py: tail text truncated to 52 chars
)

// Input is the caller-supplied context for a sweep: the book's work dir (holding
// transcripts-json/ and optionally transcripts-repaired/) and each chapter's
// duration in seconds. Every chapter present in transcripts-json/ must have a
// Durations entry - a missing one means the manifest and the transcript set have
// diverged (a re-inspect changed the chapter layout, or a stray file appeared),
// and silently defaulting it to 0 would corrupt the words-per-hour statistics
// for the whole book, so Run errors loudly instead (the Python original raised
// a KeyError on the same divergence).
type Input struct {
	WorkDir   string
	Durations map[int]float64
}

// WPHOutlier is one chapter whose words-per-hour is a statistical outlier.
type WPHOutlier struct {
	Chapter int     `json:"chapter"`
	Words   int     `json:"words"`
	Minutes float64 `json:"minutes"`
	WPH     float64 `json:"wph"`
	Z       float64 `json:"z"`
}

// RepeatedRun is a run of >= 3 consecutive identical (normalized) segments. Kind is
// KindEndFade (benign) or KindMidChapter (flagged).
type RepeatedRun struct {
	Chapter  int     `json:"chapter"`
	Kind     string  `json:"kind"`
	Length   int     `json:"length"`
	StartSec float64 `json:"start_sec"`
	Snippet  string  `json:"snippet"` // the run's first segment's ORIGINAL text, first 60 runes
}

// LowConfChapter is one chapter's low-confidence word tally (worst-N reporting).
type LowConfChapter struct {
	Chapter int `json:"chapter"`
	Low     int `json:"low"`
	Total   int `json:"total"`
}

// LowConfidence is the book-wide low-confidence summary plus the worst chapters.
// Informational only - it never affects Clean() or the retranscribe queue.
type LowConfidence struct {
	TotalLow   int              `json:"total_low"`
	TotalWords int              `json:"total_words"`
	Worst      []LowConfChapter `json:"worst"`
}

// CrossSegmentHit is a whole-chapter 6-gram loop. Pos is the percentage of the
// chapter at which the phrase first appears, or -1 when it could not be located in
// any segment. FirstSec/LastSec are the bounding segment times when located.
type CrossSegmentHit struct {
	Chapter  int      `json:"chapter"`
	Count    int      `json:"count"`
	Pos      float64  `json:"pos"`
	FirstSec *float64 `json:"first_sec,omitempty"`
	LastSec  *float64 `json:"last_sec,omitempty"`
	Phrase   string   `json:"phrase"`
}

// WithinSegmentHit is a 6-gram loop inside a single segment. Pos is the percentage
// of the chapter at which that segment starts. Unlike the cross-segment/multi-loop
// hits there is no "not located" state (the segment IS the location), but a 0
// chapter duration also yields Pos 0, indistinguishable from a genuine 0% start -
// a ported quirk that only matters for a pathological 0-length chapter.
type WithinSegmentHit struct {
	Chapter int     `json:"chapter"`
	Count   int     `json:"count"`
	Pos     float64 `json:"pos"`
	Phrase  string  `json:"phrase"`
}

// MultiLoopFinding is one of possibly several 6-gram loops in a chapter. Source is
// "repaired" or "raw" (which text layer it was found in). Pos is the percentage of
// the chapter, or -1 when not located; MidChapter is 0 <= Pos < 85.
type MultiLoopFinding struct {
	Chapter    int      `json:"chapter"`
	Count      int      `json:"count"`
	Pos        float64  `json:"pos"`
	AtSec      *float64 `json:"at_sec,omitempty"`
	Phrase     string   `json:"phrase"`
	Source     string   `json:"source"`
	MidChapter bool     `json:"mid_chapter"`
}

// TailRateHit is a chapter whose final 12 words are physically too fast. WPS/Span
// are rounded (1 and 2 decimals) to match the Python report.
type TailRateHit struct {
	Chapter int     `json:"chapter"`
	WPS     float64 `json:"wps"`
	Span    float64 `json:"span_sec"`
	Tail    string  `json:"tail"` // the joined final 12 words, first 52 runes
}

// Report is the full sweep result: the per-detector findings, the words-per-hour
// statistics, and the retranscribe queue. It is JSON-serialized as qa_report.json
// for the M5 adjudicator and the UI. All slices are non-nil (empty rather than
// null) for stable, consumer-friendly output.
type Report struct {
	Chapters          int                `json:"chapters"` // chapters considered for wph stats (excludes chapter 0)
	WPHMean           float64            `json:"wph_mean"`
	WPHStdDev         float64            `json:"wph_stddev"`
	WPHOutliers       []WPHOutlier       `json:"wph_outliers"`
	RepeatedRuns      []RepeatedRun      `json:"repeated_runs"`
	LowConfidence     LowConfidence      `json:"low_confidence"`
	CrossSegment      []CrossSegmentHit  `json:"cross_segment"`
	WithinSegment     []WithinSegmentHit `json:"within_segment"`
	MultiLoop         []MultiLoopFinding `json:"multi_loop"`
	TailRate          []TailRateHit      `json:"tail_rate"`
	RetranscribeQueue []int              `json:"retranscribe_queue"`
}

// Clean reports whether the sweep found nothing that warrants re-transcription or
// human adjudication. It is true iff there are NO wph outliers, NO mid-chapter runs
// (end fades are benign), NO cross-segment hits, NO within-segment hits, NO
// multi-loop findings and NO tail-rate hits. Low-confidence stats and end fades
// never affect cleanliness. This drives the pipeline's QAClean branch.
func (r *Report) Clean() bool {
	if len(r.WPHOutliers) > 0 {
		return false
	}
	for _, run := range r.RepeatedRuns {
		if run.Kind == KindMidChapter {
			return false
		}
	}
	return len(r.CrossSegment) == 0 &&
		len(r.WithinSegment) == 0 &&
		len(r.MultiLoop) == 0 &&
		len(r.TailRate) == 0
}

// chapterDoc is one chapter's loaded normalized transcript plus its derived full
// text and supplied duration - the shared input every detector reads.
type chapterDoc struct {
	number   int
	t        transcript.Transcript
	fulltext string // transcript.PlainText(t): the concatenation of segment texts, trimmed
	duration float64
}

// Run executes the full degeneration sweep over the normalized transcripts in
// input.WorkDir and returns the Report. It reads only transcripts-json/ (and, for
// the multi-loop detector, transcripts-repaired/); it writes nothing. A malformed
// normalized transcript is a loud error (an upstream bug), not a silent skip.
func Run(input Input) (*Report, error) {
	docs, err := loadChapters(input)
	if err != nil {
		return nil, err
	}

	chapters, mean, sd, outliers := wphOutliers(docs)
	runs := repeatedRuns(docs)

	rep := &Report{
		Chapters:          chapters,
		WPHMean:           mean,
		WPHStdDev:         sd,
		WPHOutliers:       outliers,
		RepeatedRuns:      runs,
		LowConfidence:     lowConfidence(docs),
		CrossSegment:      crossSegmentLoops(docs),
		WithinSegment:     withinSegmentLoops(docs),
		MultiLoop:         multiLoops(docs, input.WorkDir),
		TailRate:          tailRateOutliers(docs),
		RetranscribeQueue: retranscribeQueue(outliers, runs),
	}
	return rep, nil
}

// loadChapters reads and parses every transcripts-json/chNNN.json in the work dir,
// building a chapterDoc per chapter in ascending chapter order. Zero chapters is a
// loud error (nothing to sweep means the sanitizing stage has not produced output),
// as is a chapter without a duration (see Input) - the Python original crashed on
// both (statistics over an empty list; a durations KeyError), so erroring preserves
// its refusal to produce a report from inconsistent inputs.
func loadChapters(input Input) ([]chapterDoc, error) {
	jsonDir := filepath.Join(input.WorkDir, transcript.JSONDir)
	entries, err := os.ReadDir(jsonDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", transcript.JSONDir, err)
	}
	var docs []chapterDoc
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n, ok := transcript.ParseChapter(e.Name())
		if !ok || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		dur, ok := input.Durations[n]
		if !ok {
			return nil, fmt.Errorf("chapter %d has a transcript but no manifest duration - the manifest and the transcript set have diverged", n)
		}
		t, err := readTranscript(filepath.Join(jsonDir, e.Name()))
		if err != nil {
			return nil, err
		}
		docs = append(docs, chapterDoc{
			number:   n,
			t:        t,
			fulltext: transcript.PlainText(t),
			duration: dur,
		})
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("no normalized transcripts in %s", transcript.JSONDir)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].number < docs[j].number })
	return docs, nil
}

// readTranscript loads one normalized audiosilo-transcript/v1 document. The schema
// tag is verified: a syntactically-valid JSON file of the wrong shape would
// otherwise unmarshal to a zero-value transcript and silently contribute an empty
// chapter to the book statistics.
func readTranscript(path string) (transcript.Transcript, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path derives from the book's own work dir
	if err != nil {
		return transcript.Transcript{}, err
	}
	var t transcript.Transcript
	if err := json.Unmarshal(raw, &t); err != nil {
		return transcript.Transcript{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	if t.Schema != transcript.Schema {
		return transcript.Transcript{}, fmt.Errorf("%s: schema %q is not %q - not a normalized transcript",
			filepath.Base(path), t.Schema, transcript.Schema)
	}
	return t, nil
}

// retranscribeQueue is the sorted, deduped union of the wph-outlier chapters and
// the MID-CHAPTER LOOP chapters (qa_sweep.py's queue). The other detectors' findings
// feed human/agent adjudication, not this auto queue.
func retranscribeQueue(outliers []WPHOutlier, runs []RepeatedRun) []int {
	set := make(map[int]struct{})
	for _, o := range outliers {
		set[o.Chapter] = struct{}{}
	}
	for _, r := range runs {
		if r.Kind == KindMidChapter {
			set[r.Chapter] = struct{}{}
		}
	}
	queue := make([]int, 0, len(set))
	for n := range set {
		queue = append(queue, n)
	}
	sort.Ints(queue)
	return queue
}

// sampleStdDev is Python statistics.stdev: the sample standard deviation (n-1
// denominator). With fewer than 2 values it is 0 (Python raises; we degrade rather
// than panic, as a real book always has many chapters).
func sampleStdDev(vals []float64, mean float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	var ss float64
	for _, v := range vals {
		d := v - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(vals)-1))
}
