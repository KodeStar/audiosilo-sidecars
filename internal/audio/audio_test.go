package audio

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/scan"
)

// --- pure marker parsing / contiguity (no ffmpeg) ---

func TestChapterFromMarker(t *testing.T) {
	cases := []struct {
		in      string
		wantNum int
		wantTit string
		wantOk  bool
	}{
		{"Chapter 1: Troll Hunt", 1, "Troll Hunt", true},  // colon style
		{"1. Troll Hunt", 1, "Troll Hunt", true},          // bare number-dot style
		{"Chapter 4. The Deep", 4, "The Deep", true},      // dot style
		{"Chapter 7 - The Hyphen", 7, "The Hyphen", true}, // hyphen style
		{"Chapter 12", 12, "", true},                      // no title
		{"chapter 3: lowercase", 3, "lowercase", true},    // case-insensitive
		{"Chapter: 1 – New Beginnings", 1, "New Beginnings", true},
		{"Chapter: 2 — The Caravanner’s Guild", 2, "The Caravanner’s Guild", true},
		{"Chapter One", 1, "", true},         // word-number marker
		{"Chapter Twenty One", 21, "", true}, // compound word number
		{"Chapter Fifty-Four", 54, "", true}, // hyphenated word number
		{"Chapter One Hundred and Two: Return", 102, "Return", true},
		{"Chapter Forty Seven - The Gate", 47, "The Gate", true},
		{"Chapter Seventy-One – Knowing the Risks", 71, "Knowing the Risks", true},
		{"1", 1, "", true},                // bare number marker
		{"001", 1, "", true},              // zero-padded bare number
		{"064", 64, "", true},             // zero-padded, multi-digit
		{"Opening Credits", 0, "", false}, // credits excluded
		{"End Credits", 0, "", false},
		{"Prologue", 0, "", false},
		// Dialects found in the live corpus, each of which used to discard a whole
		// book's marker table for want of a punctuation separator. The literal word
		// "Chapter" is what makes a whitespace-only tail unambiguous here.
		{"Chapter 1 Suffering from Success", 1, "Suffering from Success", true},
		{"Chapter 202 Fresh Hell", 202, "Fresh Hell", true},
		{"Chapter: 1 Who Wants To Be A [Baker]?", 1, "Who Wants To Be A [Baker]?", true},
		{"Chapter 1 (Eternity's Battleship)", 1, "(Eternity's Battleship)", true},
		{"Chapter_1", 1, "", true},
		{"Chapter_12 The Deep", 12, "The Deep", true},
		{"-1-", 1, "", true},   // dash-wrapped bare number
		{"-40-", 40, "", true}, // ditto, multi-digit
		// A SPLIT chapter announces one number twice ("Chapter 10a", "Chapter 10b"),
		// which no contiguous single-number manifest can express, so it must stay
		// unrecognized and reach the mapping agent instead of silently becoming a
		// second chapter 10 - or, worse, chapter 10 titled "a". A letter straight after
		// the digits is the marker of that form, and the tail's required separator is
		// what rejects it.
		{"Chapter 10a", 0, "", false},
		// A parenthetical continuation DOES parse, because it is indistinguishable from
		// an ordinary parenthesised title ("Chapter 1 (Eternity's Battleship)") without
		// reading English. That is the safe direction: it yields a DUPLICATE chapter 61,
		// so the numbering check routes the book to the agent anyway - and the draft it
		// hands over already contains the section's audio, rather than a hole.
		{"Chapter 61 (Continued)", 61, "(Continued)", true},
		// The optional ". Title" tail must not decay into "digits then anything":
		// a bare marker is digits ONLY, and a number must lead.
		{"1a", 0, "", false},
		{"1 Some Title", 0, "", false},
		{"Disc 1 - 003", 0, "", false},
	}
	for _, c := range cases {
		num, tit, ok := chapterFromMarker(c.in)
		if ok != c.wantOk || num != c.wantNum || tit != c.wantTit {
			t.Errorf("chapterFromMarker(%q) = (%d,%q,%v), want (%d,%q,%v)",
				c.in, num, tit, ok, c.wantNum, c.wantTit, c.wantOk)
		}
	}
}

func TestContiguous(t *testing.T) {
	ch := func(nums ...int) []Chapter {
		out := make([]Chapter, len(nums))
		for i, n := range nums {
			out[i] = Chapter{Chapter: n, Start: float64(i)}
		}
		return out
	}
	cases := []struct {
		name string
		chs  []Chapter
		want bool
	}{
		{"1..3", ch(1, 2, 3), true},
		{"0..2 front matter", ch(0, 1, 2), true},
		{"gap", ch(1, 2, 4), false},
		{"starts at 2", ch(2, 3, 4), false},
		{"empty", ch(), false},
		{"single 1", ch(1), true},
		{"duplicate", ch(1, 1, 2), false},
	}
	for _, c := range cases {
		if got := contiguous(c.chs); got != c.want {
			t.Errorf("%s: contiguous = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestUnmappedSpansClassifiesAndNamesMarkers covers the span arithmetic: where a hole
// sits relative to the mapped chapters, and which raw markers occupy it. A marker
// straddling a chapter boundary is named against the span holding MOST of it, so it is
// reported once rather than against both neighbours.
func TestUnmappedSpansClassifiesAndNamesMarkers(t *testing.T) {
	chs := []Chapter{
		{Chapter: 1, Start: 100, End: 200},
		{Chapter: 2, Start: 500, End: 600},
	}
	markers := []Marker{
		{Title: "Opening Credits", Start: 0, End: 100},
		{Title: "Chapter 1", Start: 100, End: 200},
		{Title: "Interlude", Start: 200, End: 500},
		{Title: "Chapter 2", Start: 500, End: 600},
		{Title: "End Credits", Start: 600, End: 700},
	}
	got := UnmappedSpans(chs, markers, 700)
	want := []UnmappedSpan{
		{Start: 0, End: 100, Position: SpanLeading, Titles: []string{"Opening Credits"}},
		{Start: 200, End: 500, Position: SpanInterior, Titles: []string{"Interlude"}},
		{Start: 600, End: 700, Position: SpanTrailing, Titles: []string{"End Credits"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnmappedSpans =\n %+v\nwant\n %+v", got, want)
	}

	// No chapters at all: the whole recording is one span, and nothing is "leading" or
	// "trailing" anything, so it reports as interior (and is never tolerated).
	empty := UnmappedSpans(nil, markers, 700)
	if len(empty) != 1 || empty[0].Position != SpanInterior || empty[0].Duration() != 700 {
		t.Errorf("empty manifest spans = %+v, want one interior span of the whole recording", empty)
	}
	if Complete(empty) {
		t.Error("Complete() = true for a manifest that maps nothing")
	}
}

// TestCompleteSeparatesCreditsFromLostNarration pins the two thresholds. The asymmetry is
// the point: non-chapter material legitimately lives at the EDGES (credits, bloopers,
// samples), never between two chapters.
func TestCompleteSeparatesCreditsFromLostNarration(t *testing.T) {
	span := func(pos string, sec float64) UnmappedSpan {
		return UnmappedSpan{Start: 0, End: sec, Position: pos}
	}
	cases := []struct {
		name string
		span UnmappedSpan
		want bool
	}{
		{"ordinary opening credits", span(SpanLeading, 30), true},
		{"ordinary end credits", span(SpanTrailing, 60), true},
		{"a leading prologue is narration", span(SpanLeading, 1500), false},
		{"a trailing epilogue is narration", span(SpanTrailing, 250), false},
		{"an encoder artifact between chapters", span(SpanInterior, 9), true},
		{"an interior interlude is narration", span(SpanInterior, 212), false},
		// The interior bound is far tighter than the edge bound: 150s between two
		// chapters is a lost section, while the same 150s at an edge is credits.
		{"150s interior", span(SpanInterior, 150), false},
		{"150s trailing", span(SpanTrailing, 150), true},
	}
	for _, c := range cases {
		if got := Complete([]UnmappedSpan{c.span}); got != c.want {
			t.Errorf("%s: Complete(%.0fs %s) = %v, want %v", c.name, c.span.Duration(), c.span.Position, got, c.want)
		}
	}
}

// TestInterludeBetweenNumberedChaptersIsNotUsable is the headline regression, taken from
// the real "Garden of Sanctuary" (The Wandering Inn 15). Its markers number 1..9 then
// 11..20 around two unnumbered interludes and a split chapter 10a/10b. The NUMBERING
// check alone cannot see the loss - drop the 10a/10b markers and 1..9 + 11..20 is still
// a clean run in the parser's eyes - which is how 27 books lost 61 hours of narration
// with no park, no note and no agent round. Coverage is what catches it.
func TestInterludeBetweenNumberedChaptersIsNotUsable(t *testing.T) {
	work := t.TempDir()
	writeProbe(t, work, `{"format":{"duration":"5000.000"},"chapters":[
		{"start_time":"0.000","end_time":"20.000","tags":{"title":"Opening Credits"}},
		{"start_time":"20.000","end_time":"1000.000","tags":{"title":"Chapter 1"}},
		{"start_time":"1000.000","end_time":"2000.000","tags":{"title":"Chapter 2"}},
		{"start_time":"2000.000","end_time":"3000.000","tags":{"title":"Interlude"}},
		{"start_time":"3000.000","end_time":"4000.000","tags":{"title":"Chapter 3"}},
		{"start_time":"4000.000","end_time":"4980.000","tags":{"title":"Chapter 4"}},
		{"start_time":"4980.000","end_time":"5000.000","tags":{"title":"End Credits"}}]}`)

	_, stats, err := ReparseMarkerManifest(work, Manifest{Style: StyleMarkers, Duration: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Contiguous {
		t.Errorf("Contiguous = false; the recognized chapters 1..4 DO number cleanly - "+
			"coverage, not numbering, is what must fail here (stats %+v)", stats)
	}
	if stats.Complete {
		t.Error("Complete = true, but a 1000s Interlude between chapters 2 and 3 is unmapped")
	}
	if stats.Usable() {
		t.Error("Usable = true; the book must route to markers_normalizing, not split")
	}
	if desc := DescribeSpans(stats.Unmapped); !strings.Contains(desc, "Interlude") ||
		!strings.Contains(desc, "interior") {
		t.Errorf("DescribeSpans = %q, want it to name the interior Interlude", desc)
	}
}

// TestPositionalChaptersNumbersTitleOnlyTable is the "A Sacrifice of Light" regression: a
// table whose markers are pure story titles states its order in its own sequence, exactly
// as a multi-file book states it in file order. It resolves deterministically, with no
// agent round and no park.
func TestPositionalChaptersNumbersTitleOnlyTable(t *testing.T) {
	work := t.TempDir()
	writeProbe(t, work, `{"format":{"duration":"400.000"},"chapters":[
		{"start_time":"0.000","end_time":"10.000","tags":{"title":"Opening Credits"}},
		{"start_time":"10.000","end_time":"150.000","tags":{"title":"Transfer Paperwork"}},
		{"start_time":"150.000","end_time":"300.000","tags":{"title":"On the Nature of Shadows"}},
		{"start_time":"300.000","end_time":"400.000","tags":{"title":"End Credits"}}]}`)

	m, stats, err := ReparseMarkerManifest(work, Manifest{Style: StyleMarkers, Duration: 400})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Positional {
		t.Fatalf("Positional = false; a title-only table must be numbered by time order (stats %+v)", stats)
	}
	if !stats.Usable() {
		t.Errorf("Usable = false for a complete title-only table (stats %+v)", stats)
	}
	if m.ChapterCount != 4 {
		t.Errorf("chapter_count = %d, want all 4 markers kept - the credits are dropped later "+
			"by the content-driven edge classifier, never here", m.ChapterCount)
	}
	for i, ch := range m.Chapters {
		if ch.Chapter != i+1 {
			t.Errorf("chapter[%d] numbered %d, want %d", i, ch.Chapter, i+1)
		}
	}
	if m.Chapters[1].Title != "Transfer Paperwork" {
		t.Errorf("title = %q, want the whole marker label kept as the chapter title", m.Chapters[1].Title)
	}
	// Recognized stays the honest PARSER count so the metrics never claim a dialect was
	// understood; Positional is what records where the numbers came from.
	if stats.Recognized != 0 {
		t.Errorf("Recognized = %d, want 0 - chapterFromMarker understood none of these", stats.Recognized)
	}
}

// TestPositionalFallbackRefusesAGappyTable: the fallback's whole warrant is that the
// markers tile the narration, so their sequence IS the order. A table with a hole gives no
// such evidence - the hole is exactly what a human must see - so it must NOT be numbered.
func TestPositionalFallbackRefusesAGappyTable(t *testing.T) {
	work := t.TempDir()
	writeProbe(t, work, `{"format":{"duration":"400.000"},"chapters":[
		{"start_time":"0.000","end_time":"100.000","tags":{"title":"Transfer Paperwork"}},
		{"start_time":"300.000","end_time":"400.000","tags":{"title":"On the Nature of Shadows"}}]}`)

	m, stats, err := ReparseMarkerManifest(work, Manifest{Style: StyleMarkers, Duration: 400})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Positional || m.ChapterCount != 0 {
		t.Errorf("gappy title-only table was numbered anyway: positional=%v count=%d", stats.Positional, m.ChapterCount)
	}
	if !stats.NoneRecognized() {
		t.Error("NoneRecognized = false; the agent still needs to be told the parser read nothing")
	}
	if stats.Usable() {
		t.Error("Usable = true for a table with a 200s hole")
	}
}

// TestEdgeRunHoldingNarrationIsNotUsable guards the OTHER half of the loss: a Prologue or
// an Epilogue dropped at an edge is just as gone as an interlude. One real book lost its
// "Chapter 59 Part 1/2" (148 min) this way, and ~100 lost a Prologue or an Epilogue - the
// latter being exactly what the whole-book "ending" summary is written from.
func TestEdgeRunHoldingNarrationIsNotUsable(t *testing.T) {
	work := t.TempDir()
	writeProbe(t, work, `{"format":{"duration":"3000.000"},"chapters":[
		{"start_time":"0.000","end_time":"30.000","tags":{"title":"Opening Credits"}},
		{"start_time":"30.000","end_time":"1000.000","tags":{"title":"Prologue"}},
		{"start_time":"1000.000","end_time":"2000.000","tags":{"title":"Chapter 1"}},
		{"start_time":"2000.000","end_time":"2970.000","tags":{"title":"Chapter 2"}},
		{"start_time":"2970.000","end_time":"3000.000","tags":{"title":"End Credits"}}]}`)

	_, stats, err := ReparseMarkerManifest(work, Manifest{Style: StyleMarkers, Duration: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Contiguous {
		t.Errorf("Contiguous = false; chapters 1..2 number cleanly (stats %+v)", stats)
	}
	if stats.Usable() {
		t.Error("Usable = true, but a 970s Prologue sits unmapped before chapter 1")
	}

	// The same book with only credits at the edges is fine and must NOT route.
	work2 := t.TempDir()
	writeProbe(t, work2, `{"format":{"duration":"2030.000"},"chapters":[
		{"start_time":"0.000","end_time":"30.000","tags":{"title":"Opening Credits"}},
		{"start_time":"30.000","end_time":"1000.000","tags":{"title":"Chapter 1"}},
		{"start_time":"1000.000","end_time":"2000.000","tags":{"title":"Chapter 2"}},
		{"start_time":"2000.000","end_time":"2030.000","tags":{"title":"End Credits"}}]}`)
	if _, s2, err := ReparseMarkerManifest(work2, Manifest{Style: StyleMarkers, Duration: 2030}); err != nil {
		t.Fatal(err)
	} else if !s2.Usable() {
		t.Errorf("Usable = false for a book whose only unmapped audio is credits (stats %+v)", s2)
	}
}

// writeProbe drops a probe.json fixture into a work dir for the reparse tests.
func writeProbe(t *testing.T, workDir, probeJSON string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workDir, ProbeName), []byte(probeJSON), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

func TestReparseMarkerManifestRecoversColonBeforeNumber(t *testing.T) {
	work := t.TempDir()
	probe := `{
		"format":{"duration":"180.000","tags":{"title":"Mageling"}},
		"chapters":[
			{"start_time":"0.000","end_time":"10.000","tags":{"title":"Opening Credits"}},
			{"start_time":"10.000","end_time":"90.000","tags":{"title":"Chapter: 1 – New Beginnings"}},
			{"start_time":"90.000","end_time":"170.000","tags":{"title":"Chapter: 2 — The Road"}},
			{"start_time":"170.000","end_time":"180.000","tags":{"title":"End Credits"}}
		]
	}`
	writeProbe(t, work, probe)
	draft := Manifest{Source: "/books/mageling.m4b", Title: "Mageling", Style: StyleMarkers, Duration: 180}

	m, markers, err := ReparseMarkerManifest(work, draft)
	if err != nil {
		t.Fatal(err)
	}
	if !markers.Contiguous || m.ChapterCount != 2 || len(m.Chapters) != 2 {
		t.Fatalf("reparsed manifest = %+v contiguous=%v, want two contiguous chapters", m, markers.Contiguous)
	}
	if m.Chapters[0].Chapter != 1 || m.Chapters[0].Title != "New Beginnings" || m.Chapters[1].Chapter != 2 {
		t.Fatalf("reparsed chapters = %+v", m.Chapters)
	}
	stored, err := ReadManifest(work)
	if err != nil || stored.ChapterCount != 2 {
		t.Fatalf("stored manifest = %+v err=%v", stored, err)
	}
}

// TestReparseMarkerManifestRecoversBareNumberMarkers replays the real incident: three
// books whose M4B markers were nothing but zero-padded track numbers ("001".."064")
// parsed to ZERO chapters, so inspect wrote an empty draft, the state machine routed
// them to markers_normalizing, and the agent correctly declined to invent a mapping
// from bare numbers. The sequence is fully explicit, so the deterministic reparse must
// recover it titlelessly, with no agent invocation.
func TestReparseMarkerManifestRecoversBareNumberMarkers(t *testing.T) {
	work := t.TempDir()
	probe := `{
		"format":{"duration":"40.000","tags":{"title":"Inflame"}},
		"chapters":[
			{"start_time":"0.000","end_time":"10.000","tags":{"title":"001"}},
			{"start_time":"10.000","end_time":"20.000","tags":{"title":"002"}},
			{"start_time":"20.000","end_time":"30.000","tags":{"title":"003"}}
		]
	}`
	writeProbe(t, work, probe)
	// The empty draft inspect actually wrote for these books.
	draft := Manifest{Source: "/books/Inflame.m4b", Title: "Inflame", Style: StyleMarkers, Duration: 40}

	m, markers, err := ReparseMarkerManifest(work, draft)
	if err != nil {
		t.Fatal(err)
	}
	if !markers.Contiguous {
		t.Fatalf("bare-number markers must reparse contiguously, got manifest %+v", m)
	}
	if m.ChapterCount != 3 || len(m.Chapters) != 3 {
		t.Fatalf("reparsed chapter count = %d (%d chapters), want 3", m.ChapterCount, len(m.Chapters))
	}
	if m.Chapters[0].Chapter != 1 || m.Chapters[2].Chapter != 3 {
		t.Fatalf("reparsed numbering = %d..%d, want 1..3", m.Chapters[0].Chapter, m.Chapters[2].Chapter)
	}
	if m.Chapters[0].MarkerTitle != "001" || m.Chapters[0].Title != "" {
		t.Errorf("first chapter = %+v, want marker_title 001 and no title", m.Chapters[0])
	}
	stored, err := ReadManifest(work)
	if err != nil || stored.ChapterCount != 3 {
		t.Fatalf("stored manifest = %+v err=%v", stored, err)
	}
}

// TestBareNumberMarkersStillGatedByContiguity pins the guard the bare-number form does
// NOT weaken: a gappy bare-number set is still non-contiguous, so it routes to the
// markers_normalizing agent rather than being silently accepted.
func TestBareNumberMarkersStillGatedByContiguity(t *testing.T) {
	work := t.TempDir()
	probe := `{
		"format":{"duration":"40.000","tags":{"title":"Gappy"}},
		"chapters":[
			{"start_time":"0.000","end_time":"10.000","tags":{"title":"001"}},
			{"start_time":"10.000","end_time":"20.000","tags":{"title":"002"}},
			{"start_time":"20.000","end_time":"30.000","tags":{"title":"004"}}
		]
	}`
	writeProbe(t, work, probe)
	_, markers, err := ReparseMarkerManifest(work, Manifest{Style: StyleMarkers, Duration: 40})
	if err != nil {
		t.Fatal(err)
	}
	if markers.Contiguous {
		t.Fatal("a gappy bare-number marker set must NOT be reported contiguous")
	}
	// The markers WERE understood - they just do not form a run. That is the
	// vocabulary-gap condition's opposite, and the two must stay distinguishable.
	if markers.NoneRecognized() || markers.Recognized != 3 || markers.Seen != 3 {
		t.Errorf("gappy set = %+v, want 3 seen / 3 recognized and not a vocabulary gap", markers)
	}
}

// TestMarkerStatsSeparatesVocabularyGapFromMarkerless pins the distinction the stats
// exist for: BOTH cases yield an empty manifest, but one is an unfixable book and the
// other is a parser gap with a free deterministic recovery. Before this they were
// indistinguishable in the metrics, so each new marker dialect had to be rediscovered
// from a parked book.
func TestMarkerStatsSeparatesVocabularyGapFromMarkerless(t *testing.T) {
	cases := []struct {
		name           string
		chapters       string
		wantSeen       int
		wantRecognized int
		wantGap        bool
	}{
		{
			name:     "unknown dialect - a full table we cannot read",
			chapters: `{"start_time":"0.000","end_time":"10.000","tags":{"title":"Track A"}},{"start_time":"10.000","end_time":"20.000","tags":{"title":"Track B"}}`,
			wantSeen: 2, wantRecognized: 0, wantGap: true,
		},
		{
			name:     "genuinely markerless",
			chapters: ``,
			wantSeen: 0, wantRecognized: 0, wantGap: false,
		},
		{
			name:     "understood",
			chapters: `{"start_time":"0.000","end_time":"10.000","tags":{"title":"001"}}`,
			wantSeen: 1, wantRecognized: 1, wantGap: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			work := t.TempDir()
			writeProbe(t, work, `{"format":{"duration":"20.000"},"chapters":[`+c.chapters+`]}`)
			_, markers, err := ReparseMarkerManifest(work, Manifest{Style: StyleMarkers, Duration: 20})
			if err != nil {
				t.Fatal(err)
			}
			if markers.Seen != c.wantSeen || markers.Recognized != c.wantRecognized {
				t.Errorf("stats = %+v, want seen=%d recognized=%d", markers, c.wantSeen, c.wantRecognized)
			}
			if got := markers.NoneRecognized(); got != c.wantGap {
				t.Errorf("NoneRecognized() = %v, want %v (stats %+v)", got, c.wantGap, markers)
			}
		})
	}
}

// TestReparseKeepsDraftDurationWhenProbeOmitsIt guards a sharp edge: the
// markers_normalizing stage bounds the agent's corrected chapter intervals against the
// draft's Duration, so a reparse that zeroed it would reject every correct mapping the
// agent could produce. The probe still wins whenever it states a duration.
func TestReparseKeepsDraftDurationWhenProbeOmitsIt(t *testing.T) {
	work := t.TempDir()
	writeProbe(t, work, `{"chapters":[{"start_time":"0.000","end_time":"10.000","tags":{"title":"001"}}]}`)

	m, _, err := ReparseMarkerManifest(work, Manifest{Style: StyleMarkers, Duration: 42})
	if err != nil {
		t.Fatal(err)
	}
	if m.Duration != 42 {
		t.Errorf("duration = %v, want the draft's 42 preserved when the probe states none", m.Duration)
	}

	// A probe that DOES state a duration still wins.
	work2 := t.TempDir()
	writeProbe(t, work2, `{"format":{"duration":"99.000"},"chapters":[{"start_time":"0.000","end_time":"10.000","tags":{"title":"001"}}]}`)
	m2, _, err := ReparseMarkerManifest(work2, Manifest{Style: StyleMarkers, Duration: 42})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Duration != 99 {
		t.Errorf("duration = %v, want the probe's 99 to win", m2.Duration)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{
		Source: "/x/book.m4b", Title: "Book", Style: StyleMarkers, Duration: 30,
		ChapterCount: 2,
		Chapters: []Chapter{
			{Chapter: 1, Title: "A", MarkerTitle: "Chapter 1: A", Start: 0, End: 15, Duration: 15},
			{Chapter: 2, Title: "B", MarkerTitle: "Chapter 2: B", Start: 15, End: 30, Duration: 15},
		},
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.ChapterCount != 2 || got.Chapters[1].Title != "B" || got.Style != StyleMarkers {
		t.Errorf("round-trip manifest = %+v", got)
	}
}

func TestCompleteNearZeroDuration(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "tiny.flac")
	if err := os.WriteFile(small, make([]byte, 40), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	// A sub-minFlacBytes file for a normal-length chapter is NOT complete (a real
	// truncation), so resume re-splits it.
	if complete(small, 30.0) {
		t.Error("a sub-minFlacBytes file for a 30s chapter should not be complete")
	}
	// The same tiny file for a sub-second chapter IS complete, so resume does not
	// re-split a legitimately near-silent short chapter forever.
	if !complete(small, 0.3) {
		t.Error("a sub-second chapter with a non-empty file should be complete")
	}
	// A zero-byte file is never complete, even for a short chapter.
	empty := filepath.Join(dir, "empty.flac")
	if err := os.WriteFile(empty, nil, 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if complete(empty, 0.3) {
		t.Error("an empty file must not count as complete")
	}
}

// TestNaturalOrder is this repo's CONSUMER-LEVEL regression for the multi-file
// ordering contract: the comparator now lives upstream (audiosilo-meta
// pkg/scan.NaturalLess - one shared implementation, no local copy to drift), and
// this test guards both the import wiring and the ordering this package depends
// on, since the split order determines chapter numbers that spoiler-gate
// community sidecars (position.chapter). Upstream's natsort_test owns the
// exhaustive comparator cases.
func TestNaturalOrder(t *testing.T) {
	order := func(in []string) []string {
		out := append([]string(nil), in...)
		sort.SliceStable(out, func(i, j int) bool { return scan.NaturalLess(out[i], out[j]) })
		return out
	}
	cases := []struct {
		name     string
		in, want []string
	}{
		{"unpadded", []string{"ch10", "ch1", "ch2"}, []string{"ch1", "ch2", "ch10"}},
		{"mixed padding", []string{"Chapter 10.mp3", "Chapter 1.mp3", "Chapter 02.mp3"},
			[]string{"Chapter 1.mp3", "Chapter 02.mp3", "Chapter 10.mp3"}},
		{"case-insensitive words", []string{"Beta 2", "alpha 10", "alpha 2"},
			[]string{"alpha 2", "alpha 10", "Beta 2"}},
	}
	for _, c := range cases {
		if got := order(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: natural order = %v, want %v", c.name, got, c.want)
		}
	}
	// Ties are stable: equal keys keep their input order under SliceStable.
	dup := order([]string{"track 3", "track 3", "track 1"})
	if !reflect.DeepEqual(dup, []string{"track 1", "track 3", "track 3"}) {
		t.Errorf("stable tie order = %v", dup)
	}

	// And the real consumer path: audioFilesIn returns a folder's audio files in
	// that same order ("Chapter 2" before "Chapter 10", non-audio ignored).
	dir := t.TempDir()
	for _, name := range []string{"Chapter 10.mp3", "Chapter 2.mp3", "Chapter 1.mp3", "cover.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil { //nolint:gosec // test artifact
			t.Fatal(err)
		}
	}
	files, err := audioFilesIn(dir)
	if err != nil {
		t.Fatalf("audioFilesIn: %v", err)
	}
	var bases []string
	for _, f := range files {
		bases = append(bases, filepath.Base(f))
	}
	want := []string{"Chapter 1.mp3", "Chapter 2.mp3", "Chapter 10.mp3"}
	if !reflect.DeepEqual(bases, want) {
		t.Errorf("audioFilesIn order = %v, want %v", bases, want)
	}
}

// --- ffmpeg-gated inspect + split ---

func requireFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed; skipping audio integration test")
	}
	ffprobe, err = exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed; skipping audio integration test")
	}
	return ffmpeg, ffprobe
}

// genChapteredM4B builds a tiny AAC .m4b with one chapter marker per title, each
// secs long, and returns its path. It skips the test if ffmpeg is unavailable.
func genChapteredM4B(t *testing.T, ffmpeg, dir string, titles []string, secs float64) string {
	t.Helper()
	total := secs * float64(len(titles))
	var meta strings.Builder
	meta.WriteString(";FFMETADATA1\ntitle=Fixture Book\n")
	for i, title := range titles {
		start := int(float64(i) * secs * 1000)
		end := int(float64(i+1) * secs * 1000)
		meta.WriteString("[CHAPTER]\nTIMEBASE=1/1000\n")
		meta.WriteString("START=" + strconv.Itoa(start) + "\n")
		meta.WriteString("END=" + strconv.Itoa(end) + "\n")
		meta.WriteString("title=" + title + "\n")
	}
	metaPath := filepath.Join(dir, "meta.txt")
	if err := os.WriteFile(metaPath, []byte(meta.String()), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	out := filepath.Join(dir, "book.m4b")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration="+ftoa(total),
		"-i", metaPath, "-map", "0:a", "-map_metadata", "1",
		"-c:a", "aac", out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("generate fixture m4b: %v", err)
	}
	return out
}

func TestInspectAndSplitMarkers(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	// Exercise mixed marker styles in one book (all contiguous 1..3).
	book := genChapteredM4B(t, ffmpeg, dir,
		[]string{"Chapter 1: One", "Chapter 2. Two", "Chapter 3 - Three"}, 3)
	work := filepath.Join(dir, "work")

	m, markers, err := Inspect(context.Background(), book, work, ffprobe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !markers.Contiguous {
		t.Fatal("markers should be contiguous")
	}
	if m.Style != StyleMarkers || m.ChapterCount != 3 {
		t.Fatalf("manifest = %+v, want 3 markers-style chapters", m)
	}
	if m.Chapters[2].Title != "Three" {
		t.Errorf("chapter 3 title = %q, want Three", m.Chapters[2].Title)
	}
	// probe.json + manifest.json written.
	for _, f := range []string{ProbeName, ManifestName} {
		if _, err := os.Stat(filepath.Join(work, f)); err != nil {
			t.Errorf("expected %s written: %v", f, err)
		}
	}

	// Split, then verify each FLAC is mono / 16 kHz / flac.
	if err := Split(context.Background(), m, work, ffmpeg, nil); err != nil {
		t.Fatalf("Split: %v", err)
	}
	for _, ch := range m.Chapters {
		p := filepath.Join(work, ChaptersDir, ChapterFileName(ch.Chapter))
		codec, chans, rate := probeFlac(t, ffprobe, p)
		if codec != "flac" || chans != 1 || rate != 16000 {
			t.Errorf("chapter %d FLAC = codec %q, %d ch, %d Hz; want flac/1/16000", ch.Chapter, codec, chans, rate)
		}
	}
}

func TestInspectWordNumberedMarkersExcludesExtras(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	book := genChapteredM4B(t, ffmpeg, dir,
		[]string{"Opening Credits", "Chapter One – Arrival", "Chapter Two — Departure", "Bloopers", "End Credits"}, 2)
	work := filepath.Join(dir, "work")

	m, markers, err := Inspect(context.Background(), book, work, ffprobe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !markers.Contiguous {
		t.Fatal("word-numbered chapter markers should be contiguous")
	}
	if m.ChapterCount != 2 || len(m.Chapters) != 2 {
		t.Fatalf("manifest has %d chapters (%d entries), want 2", m.ChapterCount, len(m.Chapters))
	}
	if m.Chapters[0].Chapter != 1 || m.Chapters[0].Start != 2 || m.Chapters[1].Chapter != 2 || m.Chapters[1].End != 6 {
		t.Errorf("logical chapter intervals = %+v, want only Chapter One and Chapter Two", m.Chapters)
	}
}

func TestSplitResumesAfterDeletingOne(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	book := genChapteredM4B(t, ffmpeg, dir,
		[]string{"Chapter 1", "Chapter 2", "Chapter 3"}, 2)
	work := filepath.Join(dir, "work")
	m, _, err := Inspect(context.Background(), book, work, ffprobe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if err := Split(context.Background(), m, work, ffmpeg, nil); err != nil {
		t.Fatalf("Split #1: %v", err)
	}

	// Record mtimes of ch001/ch003, delete ch002, re-split, and confirm the kept
	// FLACs were NOT rewritten (same mtime) while the deleted one is restored.
	ch1 := filepath.Join(work, ChaptersDir, ChapterFileName(1))
	ch2 := filepath.Join(work, ChaptersDir, ChapterFileName(2))
	ch3 := filepath.Join(work, ChaptersDir, ChapterFileName(3))
	mt1, mt3 := mtime(t, ch1), mtime(t, ch3)
	if err := os.Remove(ch2); err != nil {
		t.Fatal(err)
	}
	// Progress must still report every chapter (skipped + redone) up to total.
	var last int
	if err := Split(context.Background(), m, work, ffmpeg, func(done, total int) {
		if total != 3 {
			t.Errorf("progress total = %d, want 3", total)
		}
		last = done
	}); err != nil {
		t.Fatalf("Split #2: %v", err)
	}
	if last != 3 {
		t.Errorf("final progress done = %d, want 3", last)
	}
	if mtime(t, ch1) != mt1 || mtime(t, ch3) != mt3 {
		t.Error("kept chapters were rewritten on resume (mtime changed)")
	}
	if !complete(ch2, m.Chapters[1].Duration) {
		t.Error("deleted chapter was not restored on resume")
	}
}

func TestInspectMultiFileStyle(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	bookDir := filepath.Join(dir, "multi")
	if err := os.MkdirAll(bookDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Two loose single-chapter files -> a files-style book.
	for _, name := range []string{"01 - Part A.mp3", "02 - Part B.mp3"} {
		cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "sine=frequency=200:duration=2",
			"-c:a", "libmp3lame", filepath.Join(bookDir, name))
		if err := cmd.Run(); err != nil {
			t.Skipf("mp3 encoder unavailable: %v", err)
		}
	}
	work := filepath.Join(dir, "work")
	m, markers, err := Inspect(context.Background(), bookDir, work, ffprobe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !markers.Contiguous || m.Style != StyleFiles || m.ChapterCount != 2 {
		t.Fatalf("multi-file manifest = %+v, want 2 files-style chapters", m)
	}
	if m.Chapters[0].FilePath == "" || m.Chapters[1].Chapter != 2 {
		t.Errorf("files-style chapters = %+v", m.Chapters)
	}
	if err := Split(context.Background(), m, work, ffmpeg, nil); err != nil {
		t.Fatalf("Split: %v", err)
	}
	for _, ch := range m.Chapters {
		p := filepath.Join(work, ChaptersDir, ChapterFileName(ch.Chapter))
		codec, chans, rate := probeFlac(t, ffprobe, p)
		if codec != "flac" || chans != 1 || rate != 16000 {
			t.Errorf("chapter %d FLAC = %q/%d/%d, want flac/1/16000", ch.Chapter, codec, chans, rate)
		}
	}
}

func TestInspectNonContiguousWritesDraftManifest(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	// A gap (1,2,4) is non-contiguous -> contiguous=false, but a DRAFT manifest is
	// written for the markers_normalizing agent stage to correct.
	book := genChapteredM4B(t, ffmpeg, dir,
		[]string{"Chapter 1", "Chapter 2", "Chapter 4"}, 2)
	work := filepath.Join(dir, "work")
	m, markers, err := Inspect(context.Background(), book, work, ffprobe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if markers.Contiguous {
		t.Error("gap markers should not be contiguous")
	}
	// The draft manifest is written (non-contiguous chapters preserved as-seen).
	if _, err := os.Stat(filepath.Join(work, ManifestName)); err != nil {
		t.Errorf("non-contiguous inspect should write a draft manifest: %v", err)
	}
	if len(m.Chapters) != 3 || Contiguous(m.Chapters) {
		t.Errorf("draft manifest should carry the 3 non-contiguous chapters, got %+v", m.Chapters)
	}
	// probe.json is still written (the record of what we saw).
	if _, err := os.Stat(filepath.Join(work, ProbeName)); err != nil {
		t.Errorf("probe.json should be written even when non-contiguous: %v", err)
	}
}

// TestInspectNumbersTitleOnlyTableEndToEnd drives the REAL ffprobe path for the condition
// that made several books look unfixable: the file HAS a complete marker table and the
// parser can read a number out of none of it. That used to leave an empty draft whose only
// route was an agent round (and, for one real book, a park). Because the markers tile the
// narration, their own sequence states the order, so inspect resolves it deterministically.
//
// The vocabulary-gap SIGNAL this test used to assert has not gone away - it now marks the
// case that genuinely still needs help, a title-only table with a hole in it, which
// TestPositionalFallbackRefusesAGappyTable covers.
func TestInspectNumbersTitleOnlyTableEndToEnd(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	book := genChapteredM4B(t, ffmpeg, dir,
		[]string{"Track A", "Track B", "Track C"}, 2)
	work := filepath.Join(dir, "work")

	m, markers, err := Inspect(context.Background(), book, work, ffprobe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !markers.Positional || !markers.Usable() {
		t.Fatalf("stats = %+v, want a positionally-numbered, usable table", markers)
	}
	if markers.Seen != 3 || markers.Recognized != 0 {
		t.Errorf("stats = %+v, want seen=3 recognized=0 (the parser really did read no number)", markers)
	}
	if m.ChapterCount != 3 || !Contiguous(m.Chapters) {
		t.Errorf("manifest = %+v, want 3 contiguously numbered chapters", m)
	}
	// probe.json still holds every marker, which is what makes a later reparse free.
	if _, err := os.Stat(filepath.Join(work, ProbeName)); err != nil {
		t.Errorf("probe.json should be written: %v", err)
	}
}

// probeFlac returns a FLAC's codec, channel count, and sample rate via ffprobe.
func probeFlac(t *testing.T, ffprobe, path string) (codec string, channels, sampleRate int) {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name,channels,sample_rate",
		"-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe flac %s: %v", path, err)
	}
	var parsed struct {
		Streams []struct {
			CodecName  string `json:"codec_name"`
			Channels   int    `json:"channels"`
			SampleRate string `json:"sample_rate"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || len(parsed.Streams) == 0 {
		t.Fatalf("parse ffprobe flac json: %v (%s)", err, out)
	}
	rate, _ := strconv.Atoi(parsed.Streams[0].SampleRate)
	return parsed.Streams[0].CodecName, parsed.Streams[0].Channels, rate
}

func mtime(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().UnixNano()
}

// SpokenChapterNumber reads the number a narrator announces at the start of a chapter. The
// live case it exists for: a book whose file 2 is an unnumbered Prologue, so file 3 announces
// "One." and the file->chapter offset is 2, not the 1 that counting files implies.
func TestSpokenChapterNumber(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opening string
		want    int
		wantOK  bool
	}{
		{"spelled", "One. Four more deaths and five days lost in zone two.", 1, true},
		{"digits", "18. Havoc paused from tinkering with his new superweapon.", 18, true},
		{"hyphenated", "Fifty-nine. Thanks for the save, Joe.", 59, true},
		{"spelled two words", "Twenty One. The gate opened.", 21, true},
		{"chapter prefix", "Chapter Twenty One. The gate opened.", 21, true},
		{"chapter prefix digits", "Chapter 7. The gate opened.", 7, true},
		{"exclamation terminator", "Three! Joe ran.", 3, true},
		{"prologue", "Prologue Hello A musical voice called over as Joe peered around", 0, false},
		{"epilogue", "Epilogue Getting out of Grandma's shoe was simple after the", 0, false},
		{"bloopers", "Bloopers! He couldn't get out more than a painted grunt.", 0, false},
		// The total-consumption rule: prose that merely STARTS with a number word is not an
		// announcement. Without it every chapter opening "One more time" would read as chapter 1.
		{"prose starting one", "One more time, Joe told himself.", 0, false},
		{"prose starting two number words", "Two hundred guards. That was the count.", 0, false},
		// A bare "Two hundred." IS parseable in isolation - deriveNarratedNumbering's requirement
		// that consecutive files announce consecutive numbers is what stops a lone parse like this
		// from shifting a book's numbering.
		{"ambiguous bare number", "Two hundred. The count was grim.", 200, true},
		{"no terminator nearby", "Joe walked into the room and looked around carefully", 0, false},
		{"empty", "", 0, false},
		{"whitespace", "   \n  ", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SpokenChapterNumber(tc.opening)
			if ok != tc.wantOK || (tc.wantOK && got != tc.want) {
				t.Errorf("SpokenChapterNumber(%q) = %d,%v want %d,%v", tc.opening, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
