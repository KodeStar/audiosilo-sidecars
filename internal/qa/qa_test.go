package qa

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// --- fixture builders (SYNTHETIC ONLY - invented text, never real book prose) ---

func seg(id int, start, end float64, text string, words ...transcript.Word) transcript.Segment {
	return transcript.Segment{ID: id, Start: start, End: end, Text: text, Words: words}
}

func wd(word string, start, end float64) transcript.Word {
	return transcript.Word{W: word, Start: start, End: end}
}

func wdp(word string, start, end, p float64) transcript.Word {
	return transcript.Word{W: word, Start: start, End: end, P: &p}
}

func mkDoc(number int, duration float64, segs ...transcript.Segment) chapterDoc {
	tr := transcript.Transcript{Schema: transcript.Schema, Segments: segs}
	return chapterDoc{number: number, t: tr, fulltext: transcript.PlainText(tr), duration: duration}
}

// wphDoc builds a chapter with a given whole-text word count and duration, without
// needing real segments (wph reads only the full text and duration).
func wphDoc(number int, duration float64, wordCount int) chapterDoc {
	return chapterDoc{
		number:   number,
		fulltext: strings.TrimSpace(strings.Repeat("word ", wordCount)),
		duration: duration,
	}
}

// distinctTokens returns n space-separated distinct tokens (w0 w1 ...) so a filler
// span never itself produces a repeated 6-gram.
func distinctTokens(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("w%d", i)
	}
	return strings.Join(parts, " ")
}

// --- repeated-segment runs (qa_sweep.py) ---

func TestRepeatedRunsMidChapterLoop(t *testing.T) {
	// The HW05 ch002 shape: a short sentence repeated ~40x well before 85%.
	var segs []transcript.Segment
	segs = append(segs, seg(0, 0, 5, "The hall was quiet."))
	for i := 0; i < 40; i++ { // 40 identical segments starting at ~35% of a 1000s chapter
		segs = append(segs, seg(i+1, 350+float64(i), 351+float64(i), "the loop text here"))
	}
	segs = append(segs, seg(41, 400, 405, "The story continued."))
	doc := mkDoc(2, 1000, segs...)

	runs := repeatedRuns([]chapterDoc{doc})
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d: %+v", len(runs), runs)
	}
	if runs[0].Kind != KindMidChapter {
		t.Errorf("kind = %q, want %q", runs[0].Kind, KindMidChapter)
	}
	if runs[0].Length != 40 {
		t.Errorf("length = %d, want 40", runs[0].Length)
	}
	if runs[0].StartSec != 350 {
		t.Errorf("start = %v, want 350", runs[0].StartSec)
	}
	// and it lands in the retranscribe queue
	q := retranscribeQueue(nil, runs)
	if len(q) != 1 || q[0] != 2 {
		t.Errorf("queue = %v, want [2]", q)
	}
}

func TestRepeatedRunsEndFadeBenign(t *testing.T) {
	segs := []transcript.Segment{
		seg(0, 0, 5, "Earlier prose."),
		seg(1, 900, 902, "same fade line"),
		seg(2, 902, 904, "same fade line"),
		seg(3, 904, 906, "same fade line"),
	}
	runs := repeatedRuns([]chapterDoc{mkDoc(1, 1000, segs...)})
	if len(runs) != 1 || runs[0].Kind != KindEndFade {
		t.Fatalf("want one end-fade, got %+v", runs)
	}
	// end fades never enter the queue
	if q := retranscribeQueue(nil, runs); len(q) != 0 {
		t.Errorf("queue = %v, want empty", q)
	}
}

func TestRepeatedRunsBoundary(t *testing.T) {
	// A 1000s chapter: a run starting at 849 is mid-chapter (< 85%); at 850 it is a
	// benign end fade (>= 85%).
	mk := func(start float64) RepeatedRun {
		segs := []transcript.Segment{
			seg(0, 0, 1, "lead in prose"),
			seg(1, start, start+1, "boundary line text"),
			seg(2, start+1, start+2, "boundary line text"),
			seg(3, start+2, start+3, "boundary line text"),
			seg(4, start+3, start+4, "different closing"),
		}
		runs := repeatedRuns([]chapterDoc{mkDoc(1, 1000, segs...)})
		if len(runs) != 1 {
			t.Fatalf("want 1 run at start %v, got %+v", start, runs)
		}
		return runs[0]
	}
	if got := mk(849).Kind; got != KindMidChapter {
		t.Errorf("start 849: kind = %q, want %q", got, KindMidChapter)
	}
	if got := mk(850).Kind; got != KindEndFade {
		t.Errorf("start 850: kind = %q, want %q", got, KindEndFade)
	}
}

func TestRepeatedRunsLengths(t *testing.T) {
	// run of 2 -> no finding
	two := []transcript.Segment{seg(0, 10, 11, "twice line"), seg(1, 11, 12, "twice line")}
	if runs := repeatedRuns([]chapterDoc{mkDoc(1, 1000, two...)}); len(runs) != 0 {
		t.Errorf("run of 2 flagged: %+v", runs)
	}
	// run of 3 -> finding
	three := []transcript.Segment{
		seg(0, 10, 11, "thrice line"), seg(1, 11, 12, "thrice line"), seg(2, 12, 13, "thrice line"),
	}
	if runs := repeatedRuns([]chapterDoc{mkDoc(1, 1000, three...)}); len(runs) != 1 || runs[0].Length != 3 {
		t.Errorf("run of 3 not flagged correctly: %+v", runs)
	}
	// run reaching the final segment -> found by the post-loop flush
	tail := []transcript.Segment{
		seg(0, 0, 1, "opening"), seg(1, 5, 6, "end run"), seg(2, 6, 7, "end run"), seg(3, 7, 8, "end run"),
	}
	if runs := repeatedRuns([]chapterDoc{mkDoc(1, 1000, tail...)}); len(runs) != 1 {
		t.Errorf("run at chapter end not flushed: %+v", runs)
	}
}

func TestRepeatedRunsSkipsChapterZero(t *testing.T) {
	segs := []transcript.Segment{
		seg(0, 10, 11, "loop"), seg(1, 11, 12, "loop"), seg(2, 12, 13, "loop"),
	}
	if runs := repeatedRuns([]chapterDoc{mkDoc(0, 1000, segs...)}); len(runs) != 0 {
		t.Errorf("chapter 0 should be excluded, got %+v", runs)
	}
}

// --- wph outliers (qa_sweep.py) ---

func TestWPHOutliersFlagsFastChapter(t *testing.T) {
	var docs []chapterDoc
	for i := 1; i <= 9; i++ {
		docs = append(docs, wphDoc(i, 3600, 9000)) // 9000 wph
	}
	docs = append(docs, wphDoc(10, 3600, 90000)) // 90000 wph - the outlier

	n, _, sd, outliers := wphOutliers(docs)
	if n != 10 {
		t.Errorf("chapters = %d, want 10", n)
	}
	if sd == 0 {
		t.Fatal("sd should be non-zero")
	}
	if len(outliers) != 1 || outliers[0].Chapter != 10 {
		t.Fatalf("want only ch10 flagged, got %+v", outliers)
	}
	if outliers[0].Z <= 2.5 {
		t.Errorf("z = %v, want > 2.5 (positive, fast chapter)", outliers[0].Z)
	}
}

func TestWPHOutliersNoStdDevNoPanic(t *testing.T) {
	// All chapters identical -> sd == 0 -> z forced to 0 -> nothing flagged, no panic.
	docs := []chapterDoc{
		wphDoc(1, 3600, 5000), wphDoc(2, 3600, 5000), wphDoc(3, 3600, 5000),
	}
	_, _, sd, outliers := wphOutliers(docs)
	if sd != 0 {
		t.Errorf("sd = %v, want 0", sd)
	}
	if len(outliers) != 0 {
		t.Errorf("outliers = %+v, want none", outliers)
	}
}

func TestWPHOutliersZeroDurationNoPanic(t *testing.T) {
	// A chapter with unknown (0) duration -> wph 0, no division panic.
	docs := []chapterDoc{
		wphDoc(1, 0, 5000), wphDoc(2, 3600, 5000), wphDoc(3, 3600, 5000),
	}
	_, _, _, outliers := wphOutliers(docs)
	_ = outliers // no panic is the assertion
}

func TestWPHOutliersExcludesChapterZero(t *testing.T) {
	docs := []chapterDoc{wphDoc(0, 3600, 999999), wphDoc(1, 3600, 5000), wphDoc(2, 3600, 5000)}
	n, _, _, _ := wphOutliers(docs)
	if n != 2 {
		t.Errorf("chapters = %d, want 2 (ch0 excluded)", n)
	}
}

// --- low-confidence words (qa_sweep.py) ---

func TestLowConfidence(t *testing.T) {
	// ch1: 2 low of 4 (nil P ignored, one >= 0.5). ch2: 0 low of 2.
	ch1 := mkDoc(1, 100, seg(0, 0, 4, "text",
		wdp("a", 0, 1, 0.2), // low
		wdp("b", 1, 2, 0.4), // low
		wdp("c", 2, 3, 0.9), // ok
		wd("d", 3, 4),       // nil P -> ignored, still counts total
	))
	ch2 := mkDoc(2, 100, seg(0, 0, 2, "text", wdp("e", 0, 1, 0.8), wdp("f", 1, 2, 0.7)))
	lc := lowConfidence([]chapterDoc{ch1, ch2})
	if lc.TotalLow != 2 || lc.TotalWords != 6 {
		t.Errorf("totals = %d/%d, want 2/6", lc.TotalLow, lc.TotalWords)
	}
	if len(lc.Worst) != 2 || lc.Worst[0].Chapter != 1 {
		t.Fatalf("worst ordering wrong: %+v", lc.Worst)
	}
	if lc.Worst[0].Low != 2 || lc.Worst[0].Total != 4 {
		t.Errorf("ch1 tally = %d/%d, want 2/4", lc.Worst[0].Low, lc.Worst[0].Total)
	}
}

func TestLowConfidenceWorstFive(t *testing.T) {
	var docs []chapterDoc
	// 7 chapters with increasing low-confidence rates; worst 5 by rate, descending.
	for i := 1; i <= 7; i++ {
		words := []transcript.Word{}
		for j := 0; j < 10; j++ {
			p := 0.9
			if j < i { // i low words out of 10 -> rate i/10
				p = 0.1
			}
			words = append(words, wdp(fmt.Sprintf("w%d", j), float64(j), float64(j)+1, p))
		}
		docs = append(docs, mkDoc(i, 100, seg(0, 0, 10, "s", words...)))
	}
	lc := lowConfidence(docs)
	if len(lc.Worst) != lowConfWorstN {
		t.Fatalf("worst len = %d, want %d", len(lc.Worst), lowConfWorstN)
	}
	wantOrder := []int{7, 6, 5, 4, 3} // highest rate first
	for i, c := range lc.Worst {
		if c.Chapter != wantOrder[i] {
			t.Errorf("worst[%d] = ch%d, want ch%d", i, c.Chapter, wantOrder[i])
		}
	}
}

// --- cross-segment loops (cross_segment_scan.py) ---

func crossDoc(number int, loopReps, filler int, start, duration float64) chapterDoc {
	text := " " + distinctTokens(filler) + " " + strings.Repeat("he ran to the old house ", loopReps)
	return mkDoc(number, duration, seg(0, start, start+10, text))
}

func TestCrossSegmentFlagsAndRepeatedRunMisses(t *testing.T) {
	doc := crossDoc(1, 5, 30, 10, 1000) // 5 loop reps (30 tok) + 30 filler = 60 tokens
	hits := crossSegmentLoops([]chapterDoc{doc})
	if len(hits) != 1 {
		t.Fatalf("want 1 cross hit, got %+v", hits)
	}
	if hits[0].Count != 5 {
		t.Errorf("count = %d, want 5", hits[0].Count)
	}
	if hits[0].Phrase != "he ran to the old house" {
		t.Errorf("phrase = %q", hits[0].Phrase)
	}
	if hits[0].FirstSec == nil || *hits[0].FirstSec != 10 {
		t.Errorf("first_sec = %v, want 10", hits[0].FirstSec)
	}
	// the near-identical-loop shape is invisible to the identical-run detector
	if runs := repeatedRuns([]chapterDoc{doc}); len(runs) != 0 {
		t.Errorf("repeated-run detector should miss the cross-segment loop, got %+v", runs)
	}
}

func TestCrossSegmentThreshold(t *testing.T) {
	// 4 reps + 40 filler = 64 tokens (>= 50) but the 6-gram repeats only 4x -> not flagged.
	if hits := crossSegmentLoops([]chapterDoc{crossDoc(1, 4, 40, 0, 1000)}); len(hits) != 0 {
		t.Errorf("count 4 should not flag, got %+v", hits)
	}
	// 5 reps -> flagged (covered above); assert count exactly at threshold.
	if hits := crossSegmentLoops([]chapterDoc{crossDoc(1, 5, 30, 0, 1000)}); len(hits) != 1 {
		t.Errorf("count 5 should flag, got %+v", hits)
	}
}

func TestCrossSegmentSkipsShortChapter(t *testing.T) {
	// 5 loop reps (30 tok) + 10 filler = 40 tokens < 50 -> skipped despite the loop.
	if hits := crossSegmentLoops([]chapterDoc{crossDoc(1, 5, 10, 0, 1000)}); len(hits) != 0 {
		t.Errorf("< 50 tokens must be skipped, got %+v", hits)
	}
}

func TestCrossSegmentScansChapterZero(t *testing.T) {
	// Asymmetry: cross-segment INCLUDES chapter 0 (unlike wph / within / repeated-run).
	if hits := crossSegmentLoops([]chapterDoc{crossDoc(0, 5, 30, 0, 1000)}); len(hits) != 1 {
		t.Errorf("chapter 0 must be scanned, got %+v", hits)
	}
}

// --- within-segment loops (within_segment_scan.py) ---

func TestWithinSegmentThreshold(t *testing.T) {
	// One segment containing a 6-gram repeated 8x -> flagged.
	eight := " " + strings.Repeat("aa bb cc dd ee ff ", 8)
	hits := withinSegmentLoops([]chapterDoc{mkDoc(1, 1000, seg(0, 500, 560, eight))})
	if len(hits) != 1 || hits[0].Count != 8 {
		t.Fatalf("8x should flag with count 8, got %+v", hits)
	}
	if hits[0].Phrase != "aa bb cc dd ee ff" {
		t.Errorf("phrase = %q", hits[0].Phrase)
	}
	if hits[0].Pos != 50 { // 500 / 1000 * 100
		t.Errorf("pos = %v, want 50", hits[0].Pos)
	}
	// 7x -> just under threshold, not flagged (the HW04 ch012 near-miss).
	seven := " " + strings.Repeat("aa bb cc dd ee ff ", 7)
	if h := withinSegmentLoops([]chapterDoc{mkDoc(1, 1000, seg(0, 0, 60, seven))}); len(h) != 0 {
		t.Errorf("7x should not flag, got %+v", h)
	}
}

func TestWithinSegmentSkipsShortSegment(t *testing.T) {
	// A segment under 12 tokens is skipped even if it repeats a gram.
	short := " aa bb cc aa bb cc" // 6 tokens
	if h := withinSegmentLoops([]chapterDoc{mkDoc(1, 1000, seg(0, 0, 5, short))}); len(h) != 0 {
		t.Errorf("segment < 12 tokens must be skipped, got %+v", h)
	}
}

// --- multi-loop scan (multi_loop_scan.py) ---

func TestMultiLoopTwoDistinctLoops(t *testing.T) {
	// Two loops with disjoint word sets in one chapter -> both reported (the raison
	// d'etre), while overlapping shingles of each collapse to one finding.
	text := " " + distinctTokens(20) + " " +
		strings.Repeat("alpha bravo ", 8) + strings.Repeat("charlie delta ", 8)
	doc := mkDoc(5, 1000, seg(0, 100, 900, text))
	got := multiLoops([]chapterDoc{doc}, t.TempDir())
	if len(got) != 2 {
		t.Fatalf("want 2 distinct loops, got %d: %+v", len(got), got)
	}
	sets := map[string]bool{}
	for _, f := range got {
		sets[wordSetKey(strings.Fields(f.Phrase))] = true
		if f.Source != "raw" {
			t.Errorf("source = %q, want raw", f.Source)
		}
		if !f.MidChapter {
			t.Errorf("finding should be mid-chapter: %+v", f)
		}
	}
	if len(sets) != 2 {
		t.Errorf("want 2 distinct word sets, got %d", len(sets))
	}
}

func TestMultiLoopPrefersRepairedLayer(t *testing.T) {
	work := t.TempDir()
	repDir := filepath.Join(work, transcript.RepairedDir)
	if err := os.MkdirAll(repDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// The raw/normalized text has no loop; the repaired layer does.
	repaired := distinctTokens(40) + " " + strings.Repeat("alpha bravo ", 8)
	if err := os.WriteFile(filepath.Join(repDir, transcript.TextName(5)), []byte(repaired), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := mkDoc(5, 1000, seg(0, 100, 900, " "+distinctTokens(60))) // raw: no loop
	got := multiLoops([]chapterDoc{doc}, work)
	if len(got) != 1 {
		t.Fatalf("want 1 loop from the repaired layer, got %+v", got)
	}
	if got[0].Source != "repaired" {
		t.Errorf("source = %q, want repaired", got[0].Source)
	}
}

func TestNormalizeASCIIvsUnicode(t *testing.T) {
	// The ASCII normalizer (multi-loop) drops an accented letter; the Unicode one
	// (repeated-run) keeps it lowercased - the two genuinely differ.
	if got := normalizeASCII("Café, THE-end!"); got != "caf the end" {
		t.Errorf("normalizeASCII = %q, want %q", got, "caf the end")
	}
	if got := normalizeUnicode("Café, THE-end!"); got != "café the end" {
		t.Errorf("normalizeUnicode = %q, want %q", got, "café the end")
	}
}

// --- tail rate (tail_rate_scan.py) ---

func tailDoc(number int, words []transcript.Word) chapterDoc {
	toks := make([]string, len(words))
	for i, w := range words {
		toks[i] = w.W
	}
	return mkDoc(number, 500, seg(0, 0, 200, " "+strings.Join(toks, " "), words...))
}

func TestTailRateFlagsCrammedTailOthersMiss(t *testing.T) {
	var words []transcript.Word
	for i := 0; i < 8; i++ { // 8 normal words
		words = append(words, wd("word", float64(i), float64(i)+0.5))
	}
	for i := 0; i < 12; i++ { // 12 one-word-loop words crammed into 0.16s (impossible)
		words = append(words, wd("art", 100.0+float64(i)*0.01, 100.0+float64(i)*0.01+0.005))
	}
	// force the exact tail span 100.00 -> 100.16
	words[len(words)-1] = wd("art", 100.15, 100.16)
	doc := tailDoc(3, words)

	tails := tailRateOutliers([]chapterDoc{doc})
	if len(tails) != 1 {
		t.Fatalf("tail-rate should flag, got %+v", tails)
	}
	if tails[0].WPS <= tailMaxWPS {
		t.Errorf("wps = %v, want > %v", tails[0].WPS, tailMaxWPS)
	}
	// No 6-gram detector can catch a one-word loop / short chapter.
	if h := crossSegmentLoops([]chapterDoc{doc}); len(h) != 0 {
		t.Errorf("cross should miss, got %+v", h)
	}
	if h := withinSegmentLoops([]chapterDoc{doc}); len(h) != 0 {
		t.Errorf("within should miss, got %+v", h)
	}
	if h := multiLoops([]chapterDoc{doc}, t.TempDir()); len(h) != 0 {
		t.Errorf("multi should miss, got %+v", h)
	}
}

func TestTailRateNormalTailNotFlagged(t *testing.T) {
	var words []transcript.Word
	for i := 0; i < 5; i++ {
		words = append(words, wd("intro", float64(i), float64(i)+0.5))
	}
	// 12 words evenly over 4.8s -> 2.5 w/s (normal narration).
	for i := 0; i < 12; i++ {
		words = append(words, wd("real", 10.0+float64(i)*0.4, 10.0+float64(i)*0.4+0.3))
	}
	words[len(words)-1] = wd("real", 14.5, 14.8)
	if tails := tailRateOutliers([]chapterDoc{tailDoc(3, words)}); len(tails) != 0 {
		t.Errorf("normal ~2.5 w/s tail flagged: %+v", tails)
	}
}

func TestTailRateSkipsShortChapter(t *testing.T) {
	var words []transcript.Word
	for i := 0; i < 16; i++ { // < tailWords + 5 (17)
		words = append(words, wd("w", float64(i)*0.01, float64(i)*0.01+0.005))
	}
	if tails := tailRateOutliers([]chapterDoc{tailDoc(3, words)}); len(tails) != 0 {
		t.Errorf("chapter with < 17 words must be skipped, got %+v", tails)
	}
}

func TestTailRateSpanFloor(t *testing.T) {
	var words []transcript.Word
	for i := 0; i < 5; i++ {
		words = append(words, wd("w", float64(i), float64(i)+0.5))
	}
	for i := 0; i < 12; i++ { // all at the same instant -> span floors to 0.001, no div by zero
		words = append(words, wd("z", 50, 50))
	}
	tails := tailRateOutliers([]chapterDoc{tailDoc(3, words)})
	if len(tails) != 1 {
		t.Fatalf("zero-span tail should flag via the floor, got %+v", tails)
	}
	if tails[0].Span != round2(tailSpanFloor) {
		t.Errorf("span = %v, want %v (the floor)", tails[0].Span, round2(tailSpanFloor))
	}
}

// --- retranscribe queue ---

func TestRetranscribeQueueUnionSortDedup(t *testing.T) {
	outliers := []WPHOutlier{{Chapter: 5}, {Chapter: 2}}
	runs := []RepeatedRun{
		{Chapter: 2, Kind: KindMidChapter},
		{Chapter: 9, Kind: KindMidChapter},
		{Chapter: 3, Kind: KindEndFade}, // end fades excluded
	}
	got := retranscribeQueue(outliers, runs)
	want := []int{2, 5, 9}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("queue = %v, want %v", got, want)
	}
}

// --- Clean() ---

func TestClean(t *testing.T) {
	cases := []struct {
		name string
		r    Report
		want bool
	}{
		{"empty", Report{}, true},
		{"wph outlier", Report{WPHOutliers: []WPHOutlier{{Chapter: 1}}}, false},
		{"mid-chapter run", Report{RepeatedRuns: []RepeatedRun{{Kind: KindMidChapter}}}, false},
		{"end-fade only", Report{RepeatedRuns: []RepeatedRun{{Kind: KindEndFade}}}, true},
		{"cross", Report{CrossSegment: []CrossSegmentHit{{Chapter: 1}}}, false},
		{"within", Report{WithinSegment: []WithinSegmentHit{{Chapter: 1}}}, false},
		{"multi", Report{MultiLoop: []MultiLoopFinding{{Chapter: 1}}}, false},
		{"tail", Report{TailRate: []TailRateHit{{Chapter: 1}}}, false},
		{"low-confidence only", Report{LowConfidence: LowConfidence{TotalLow: 5, TotalWords: 10, Worst: []LowConfChapter{{Chapter: 1, Low: 5, Total: 10}}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.Clean(); got != tc.want {
				t.Errorf("Clean() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- pyRepr (matches Python repr() so the golden test can compare snippets) ---

func TestPyRepr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hump grinned.", "'Hump grinned.'"},
		{" leading space", "' leading space'"},
		{"it's", `"it's"`},
		{`say "hi"`, `'say "hi"'`},
		{`both ' and "`, `'both \' and "'`},
		{"tab\tafter", `'tab\tafter'`},
		{`back\slash`, `'back\\slash'`},
	}
	for _, c := range cases {
		if got := pyRepr(c.in); got != c.want {
			t.Errorf("pyRepr(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// --- Run + WriteReport (end to end over a synthetic on-disk work dir) ---

func writeNormalized(t *testing.T, jsonDir string, tr transcript.Transcript) {
	t.Helper()
	if err := os.MkdirAll(jsonDir, 0o750); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jsonDir, transcript.JSONName(tr.Chapter)), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunAndWriteReport(t *testing.T) {
	work := t.TempDir()
	jsonDir := filepath.Join(work, transcript.JSONDir)

	// ch0 (recap, excluded from wph), ch1 clean, ch2 a mid-chapter loop.
	writeNormalized(t, jsonDir, transcript.Transcript{Chapter: 0, Schema: transcript.Schema,
		Segments: []transcript.Segment{seg(0, 0, 5, " a recap of earlier events unfolds")}})
	writeNormalized(t, jsonDir, transcript.Transcript{Chapter: 1, Schema: transcript.Schema,
		Segments: []transcript.Segment{seg(0, 0, 5, " the story proceeds calmly and clearly")}})
	var loop []transcript.Segment
	loop = append(loop, seg(0, 0, 5, " chapter two opens"))
	for i := 0; i < 6; i++ {
		loop = append(loop, seg(i+1, 100+float64(i), 101+float64(i), "looping phrase here"))
	}
	loop = append(loop, seg(7, 200, 205, " and then it ends"))
	writeNormalized(t, jsonDir, transcript.Transcript{Chapter: 2, Schema: transcript.Schema, Segments: loop})

	in := Input{WorkDir: work, Durations: map[int]float64{0: 300, 1: 300, 2: 1000}}
	rep, err := Run(in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Chapters != 2 {
		t.Errorf("chapters = %d, want 2 (ch0 excluded)", rep.Chapters)
	}
	if len(rep.RepeatedRuns) != 1 || rep.RepeatedRuns[0].Kind != KindMidChapter {
		t.Fatalf("want one mid-chapter run, got %+v", rep.RepeatedRuns)
	}
	if len(rep.RetranscribeQueue) != 1 || rep.RetranscribeQueue[0] != 2 {
		t.Errorf("queue = %v, want [2]", rep.RetranscribeQueue)
	}
	if rep.Clean() {
		t.Error("report with a mid-chapter loop should not be Clean")
	}

	if err := WriteReport(work, rep); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	// qa_report.json round-trips.
	raw, err := os.ReadFile(filepath.Join(work, ReportJSONName))
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("qa_report.json does not round-trip: %v", err)
	}
	if fmt.Sprint(back.RetranscribeQueue) != fmt.Sprint(rep.RetranscribeQueue) {
		t.Errorf("round-trip queue = %v, want %v", back.RetranscribeQueue, rep.RetranscribeQueue)
	}

	// qa_report.md carries the expected section headers and lines.
	md, err := os.ReadFile(filepath.Join(work, ReportMDName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# QA report",
		"chapters: 2",
		"## wph outliers (|z| > 2.5)",
		"## repeated-segment runs (>=3 identical normalized segments)",
		"ch002 MID-CHAPTER LOOP", // the md renders the historical label, not the Kind enum

		"## low-confidence words (<0.5)",
		"## retranscribe queue",
		"- [2]",
		"## cross-segment loops (6-gram repeated >=5x per chapter)",
		"## within-segment loops (6-gram repeated >=8x in one segment)",
		"## multi-loop scan (every 6-gram repeated >=5x per chapter)",
		"## tail-rate outliers",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("qa_report.md missing %q\n---\n%s", want, md)
		}
	}
}

func TestRunMissingJSONDir(t *testing.T) {
	if _, err := Run(Input{WorkDir: t.TempDir()}); err == nil {
		t.Error("Run should error when transcripts-json/ is absent")
	}
}

// The loud-error contract of loadChapters: a transcript without a manifest
// duration, an empty transcripts-json/, and a wrong-schema JSON file are all
// errors, never silent zero-value chapters that would corrupt the book stats.
func TestRunLoudErrors(t *testing.T) {
	t.Run("missing duration", func(t *testing.T) {
		work := t.TempDir()
		writeNormalized(t, filepath.Join(work, transcript.JSONDir), transcript.Transcript{
			Chapter: 1, Schema: transcript.Schema,
			Segments: []transcript.Segment{seg(0, 0, 5, " some narration")}})
		_, err := Run(Input{WorkDir: work, Durations: map[int]float64{}})
		if err == nil || !strings.Contains(err.Error(), "diverged") {
			t.Errorf("missing duration should error loudly, got %v", err)
		}
	})
	t.Run("empty transcripts-json", func(t *testing.T) {
		work := t.TempDir()
		if err := os.MkdirAll(filepath.Join(work, transcript.JSONDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Run(Input{WorkDir: work}); err == nil {
			t.Error("an empty transcripts-json/ must not produce a (clean) report")
		}
	})
	t.Run("wrong schema", func(t *testing.T) {
		work := t.TempDir()
		jsonDir := filepath.Join(work, transcript.JSONDir)
		if err := os.MkdirAll(jsonDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(jsonDir, transcript.JSONName(1)),
			[]byte(`{"unrelated":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Run(Input{WorkDir: work, Durations: map[int]float64{1: 60}})
		if err == nil || !strings.Contains(err.Error(), "schema") {
			t.Errorf("a wrong-shape JSON file should be a schema error, got %v", err)
		}
	})
}
