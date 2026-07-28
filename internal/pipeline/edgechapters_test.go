package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// ec builds a PROBED edgeChapter with a present transcript of the given word count and no
// recorded duration (0, which edgeDurationShort treats as short, so word count alone decides).
func ec(chapter, words int) edgeChapter {
	return edgeChapter{Chapter: chapter, Words: words, HasTranscript: true, Probed: true}
}

// ecDur is ec with an explicit manifest duration, for the short-duration corroboration.
func ecDur(chapter, words int, durationSec float64) edgeChapter {
	c := ec(chapter, words)
	c.DurationSec = durationSec
	return c
}

// bigNarrative builds a run of clearly-narrative chapters [from,to] (well over the
// threshold), the surrounding body of a real book.
func bigNarrative(from, to int) []edgeChapter {
	var out []edgeChapter
	for k := from; k <= to; k++ {
		out = append(out, ec(k, 3000))
	}
	return out
}

func TestClassifyEdgeChapters(t *testing.T) {
	// The two real incident shapes, generated the way inspectFiles numbers files 1..N.
	realShape := func(files int) []edgeChapter {
		chs := []edgeChapter{ec(1, 3)} // the 3-word "This is Audible." intro file
		chs = append(chs, bigNarrative(2, files-1)...)
		chs = append(chs, ec(files, 80)) // closing credits
		return chs
	}

	cases := []struct {
		name         string
		in           []edgeChapter
		logical      int
		leading      []int
		trailing     []int
		wantNote     bool
		noteContains []string
	}{
		{
			name: "78-file book with intro + credits -> logical 76, excludes 1 and 78",
			in:   realShape(78), logical: 76, leading: []int{1}, trailing: []int{78},
			wantNote: true, noteContains: []string{"Audio files 1 and 78 are non-narrative", "opening announcement / closing credits", "1-76 as spoken"},
		},
		{
			name: "100-file book -> logical 98, excludes 1 and 100",
			in:   realShape(100), logical: 98, leading: []int{1}, trailing: []int{100},
			wantNote: true, noteContains: []string{"Audio files 1 and 100 are non-narrative", "1-98 as spoken"},
		},
		{
			name: "all files above threshold -> no exclusions, empty note",
			in:   bigNarrative(1, 5), logical: 5, leading: nil, trailing: nil, wantNote: false,
		},
		{
			name:    "an interior small chapter is NEVER excluded",
			in:      []edgeChapter{ec(1, 3000), ec(2, 3000), ec(3, 40), ec(4, 3000)},
			logical: 4, leading: nil, trailing: nil, wantNote: false,
		},
		{
			name:    "all chapters below threshold -> degenerate fallback, no exclusions",
			in:      []edgeChapter{ec(1, 5), ec(2, 8), ec(3, 4)},
			logical: 3, leading: nil, trailing: nil, wantNote: false,
		},
		{
			name: "a missing transcript counts as narrative and is not excluded",
			// ch1 has NO transcript (defensive narrative) so the leading run is empty;
			// only the trailing 3-word credits file is excluded.
			in:      []edgeChapter{{Chapter: 1, Words: 0, HasTranscript: false}, ec(2, 3000), ec(3, 3000), ec(4, 3)},
			logical: 3, leading: nil, trailing: []int{4},
			wantNote: true, noteContains: []string{"Audio file 4 is non-narrative", "closing credits", "1-3 as spoken"},
		},
		{
			name:    "leading intro only -> excludes 1, opening announcement",
			in:      append([]edgeChapter{ec(1, 3)}, bigNarrative(2, 4)...),
			logical: 3, leading: []int{1}, trailing: nil,
			wantNote: true, noteContains: []string{"Audio file 1 is non-narrative", "opening announcement", "1-3 as spoken"},
		},
		{
			name:    "two leading intro files -> excludes 1 and 2",
			in:      append([]edgeChapter{ec(1, 3), ec(2, 10)}, bigNarrative(3, 6)...),
			logical: 4, leading: []int{1, 2}, trailing: nil,
			wantNote: true, noteContains: []string{"Audio files 1 and 2 are non-narrative"},
		},
		{
			name: "empty input -> zero logical, empty note",
			in:   nil, logical: 0, leading: nil, trailing: nil, wantNote: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyEdgeChapters(tc.in)
			if got.LogicalCount != tc.logical {
				t.Errorf("LogicalCount = %d, want %d", got.LogicalCount, tc.logical)
			}
			if !reflect.DeepEqual(got.ExcludedLeading, tc.leading) {
				t.Errorf("ExcludedLeading = %v, want %v", got.ExcludedLeading, tc.leading)
			}
			if !reflect.DeepEqual(got.ExcludedTrailing, tc.trailing) {
				t.Errorf("ExcludedTrailing = %v, want %v", got.ExcludedTrailing, tc.trailing)
			}
			if tc.wantNote && got.EdgeNote == "" {
				t.Fatal("EdgeNote is empty, want a note")
			}
			if !tc.wantNote && got.EdgeNote != "" {
				t.Fatalf("EdgeNote = %q, want empty (no exclusions)", got.EdgeNote)
			}
			for _, sub := range tc.noteContains {
				if !strings.Contains(got.EdgeNote, sub) {
					t.Errorf("EdgeNote %q missing %q", got.EdgeNote, sub)
				}
			}
			// House style: the note never carries an em dash.
			if hasEmDash(got.EdgeNote) {
				t.Errorf("EdgeNote contains an em dash: %q", got.EdgeNote)
			}
		})
	}
}

// shortRun builds a run of short PROBED chapters [from,to] (3 words each, 0 duration => short),
// the shape of a would-be edge run.
func shortRun(from, to int) []edgeChapter {
	var out []edgeChapter
	for k := from; k <= to; k++ {
		out = append(out, ec(k, 3))
	}
	return out
}

// TestClassifyEdgeChaptersProbedGate is Fix 5(a): an UNPROBED edge chapter (a large book's
// interior sentinel, Probed:false) is never non-narrative even with a low word count, so it can
// never bound-blank a book whose interior was not word-counted. Only probed short-word edges are
// excluded.
func TestClassifyEdgeChaptersProbedGate(t *testing.T) {
	// ch1 has 3 words + a transcript but is UNPROBED -> treated as narrative, so no leading run.
	in := []edgeChapter{{Chapter: 1, Words: 3, HasTranscript: true, Probed: false}, ec(2, 3000), ec(3, 3000)}
	got := classifyEdgeChapters(in)
	if got.LogicalCount != 3 || got.ExcludedLeading != nil || got.ExcludedTrailing != nil {
		t.Errorf("unprobed short leading chapter was excluded: %+v", got)
	}
}

// TestClassifyEdgeChaptersProbeSaturation is Fix 5(b): a leading OR trailing run reaching the
// probe depth (no narrative chapter found within the probe window) is unreliable, so the
// classifier returns no exclusions (raw count) rather than blanking most of the book. This is the
// 17-chapter-of-1-minute-chapters shape whose degenerate all-short fallback can never fire.
func TestClassifyEdgeChaptersProbeSaturation(t *testing.T) {
	t.Run("leading run reaches probe depth", func(t *testing.T) {
		in := append(shortRun(1, edgeProbeDepth), bigNarrative(edgeProbeDepth+1, edgeProbeDepth+6)...)
		got := classifyEdgeChapters(in)
		if got.LogicalCount != len(in) || got.ExcludedLeading != nil || got.ExcludedTrailing != nil || got.EdgeNote != "" {
			t.Errorf("a probe-depth leading run should saturate to no exclusions; got %+v", got)
		}
	})
	t.Run("trailing run reaches probe depth", func(t *testing.T) {
		in := append(bigNarrative(1, 6), shortRun(7, 6+edgeProbeDepth)...)
		got := classifyEdgeChapters(in)
		if got.LogicalCount != len(in) || got.ExcludedLeading != nil || got.ExcludedTrailing != nil {
			t.Errorf("a probe-depth trailing run should saturate to no exclusions; got %+v", got)
		}
	})
}

// TestClassifyBookEdgesUniformShortBookSaturates is the IO-level probe-saturation regression: a
// 17-chapter book of uniformly sub-threshold short chapters (the first/last edgeProbeDepth probe
// runs saturate) must NOT exclude 16 real chapters - classifyBookEdges returns the raw count.
func TestClassifyBookEdgesUniformShortBookSaturates(t *testing.T) {
	work := t.TempDir()
	counts := make([]int, 17) // 17 short chapters
	for i := range counts {
		counts[i] = 10
	}
	seedWordsManifest(t, work, audio.StyleFiles, counts)
	class, err := classifyBookEdges(work)
	if err != nil {
		t.Fatalf("classifyBookEdges: %v", err)
	}
	if class.LogicalCount != 17 || class.ExcludedLeading != nil || class.ExcludedTrailing != nil {
		t.Errorf("a uniformly-short 17-chapter book was blanked by probe saturation: %+v", class)
	}
}

// TestClassifyEdgeChaptersDurationCorroboration is Fix 6(a): a sub-threshold-word EDGE chapter is
// excluded ONLY when its duration is also short. A brief (few-word) but MINUTES-long chapter is a
// real chapter (e.g. a sparse epilogue) and must survive; the incident's 2s intro / 65s credits
// are seconds and are excluded. An unknown (0) duration falls back to the word-only decision.
func TestClassifyEdgeChaptersDurationCorroboration(t *testing.T) {
	t.Run("short words + short duration is excluded", func(t *testing.T) {
		in := []edgeChapter{ecDur(1, 3, 30), ec(2, 3000), ec(3, 3000)}
		got := classifyEdgeChapters(in)
		if got.LogicalCount != 2 || !reflect.DeepEqual(got.ExcludedLeading, []int{1}) {
			t.Errorf("a short-word short-duration intro should be excluded; got %+v", got)
		}
	})
	t.Run("short words + LONG duration is a real chapter, not excluded", func(t *testing.T) {
		in := []edgeChapter{ecDur(1, 3, 300), ec(2, 3000), ec(3, 3000)}
		got := classifyEdgeChapters(in)
		if got.LogicalCount != 3 || got.ExcludedLeading != nil {
			t.Errorf("a minutes-long few-word chapter must not be excluded/mislabeled credits; got %+v", got)
		}
	})
	t.Run("unknown (zero) duration falls back to word-only", func(t *testing.T) {
		in := []edgeChapter{ecDur(1, 3, 0), ec(2, 3000), ec(3, 3000)}
		got := classifyEdgeChapters(in)
		if got.LogicalCount != 2 || !reflect.DeepEqual(got.ExcludedLeading, []int{1}) {
			t.Errorf("an unknown-duration short intro should keep the word-only exclusion; got %+v", got)
		}
	})
}

// TestComposeChunkAndAssembleNotes is Fix 1: the fact-pass CHUNK note keeps headings on file
// numbers (no renumbering), while the ASSEMBLE note is the ONE renumbering boundary and states
// the concrete file->spoken mapping when there is a leading exclusion.
func TestComposeChunkAndAssembleNotes(t *testing.T) {
	// A leading exclusion (file 1 = intro) plus a trailing credits file: 76 logical chapters.
	class := classifyEdgeChapters(append(append([]edgeChapter{ec(1, 3)}, bigNarrative(2, 77)...), ec(78, 80)))
	if class.ChunkNote == "" || class.AssembleNote == "" {
		t.Fatalf("chunk/assemble notes empty for an edge book: %+v", class)
	}
	// The chunk note pins headings to FILE numbers and never renumbers.
	for _, sub := range []string{"audio-FILE numbers", "never", "renumber", "spoken", "no story facts"} {
		if !strings.Contains(class.ChunkNote, sub) {
			t.Errorf("chunk note missing %q: %q", sub, class.ChunkNote)
		}
	}
	// The assemble note carries the concrete mapping: file N -> spoken N-1, keyed to spoken 1-76.
	for _, sub := range []string{"renumbering boundary", "N-1", "SPOKEN story chapter numbers 1-76", "subtracting 1"} {
		if !strings.Contains(class.AssembleNote, sub) {
			t.Errorf("assemble note missing %q: %q", sub, class.AssembleNote)
		}
	}
	if hasEmDash(class.ChunkNote) || hasEmDash(class.AssembleNote) {
		t.Errorf("chunk/assemble note contains an em dash")
	}

	// Trailing-only exclusion: no renumbering (file numbers already equal spoken numbers).
	trailingOnly := classifyEdgeChapters(append(bigNarrative(1, 3), ec(4, 3)))
	if strings.Contains(trailingOnly.AssembleNote, "renumbering boundary") {
		t.Errorf("a trailing-only book must not claim a renumbering boundary: %q", trailingOnly.AssembleNote)
	}
	if !strings.Contains(trailingOnly.AssembleNote, "already the spoken story-chapter numbers") {
		t.Errorf("a trailing-only assemble note should say no renumbering is needed: %q", trailingOnly.AssembleNote)
	}
}

// TestComposeEdgeNoteEmptyWhenNothingExcluded pins the exact note wording and the
// empty-string contract when there is nothing to exclude.
func TestComposeEdgeNoteEmptyWhenNothingExcluded(t *testing.T) {
	if got := composeEdgeNote(76, nil, nil, nil, nil); got != "" {
		t.Errorf("note with no exclusions = %q, want empty", got)
	}
	want := "Audio files 1 and 78 are non-narrative (opening announcement / closing credits), " +
		"not story chapters. The work's logical story chapters are 1-76 as spoken in the " +
		"narration; facts and positions use those spoken chapter numbers."
	if got := composeEdgeNote(76, []int{1}, []int{78}, nil, nil); got != want {
		t.Errorf("note = %q\nwant %q", got, want)
	}
}

// writeChapterWords writes a chapter's text layer with the given word count (0 skips,
// leaving no transcript file so the classifier treats it as narrative).
func writeChapterWords(t *testing.T, work string, chapter, words int) {
	t.Helper()
	if words == 0 {
		return
	}
	text := strings.TrimSpace(strings.Repeat("word ", words))
	if err := transcript.WriteText(filepath.Join(work, transcript.TextDir), chapter, text); err != nil {
		t.Fatal(err)
	}
}

// seedWordsManifest writes a manifest of the given style with one chapter per entry
// in wordCounts (chapters 1..N) plus each chapter's text-layer transcript. A
// bare-number markers book (the live incident shape) and a files book must classify
// identically - the classifier is content-driven, not style-driven.
func seedWordsManifest(t *testing.T, work, style string, wordCounts []int) {
	t.Helper()
	m := audio.Manifest{Source: "/x/book", Style: style, ChapterCount: len(wordCounts)}
	for i, w := range wordCounts {
		ch := i + 1
		m.Chapters = append(m.Chapters, audio.Chapter{Chapter: ch, FilePath: filepath.Join("/x/book", "part.mp3")})
		writeChapterWords(t, work, ch, w)
	}
	writeManifestStruct(t, work, m)
}

func TestClassifyManifestEdgesReadsTranscripts(t *testing.T) {
	// Both a files book and a bare-number markers book (the live incident shape) must
	// exclude a 3-word leading intro and a short trailing credits file -> logical n-2.
	for _, style := range []string{audio.StyleFiles, audio.StyleMarkers} {
		t.Run(style, func(t *testing.T) {
			work := t.TempDir()
			// chapter 1 = 3-word intro, 2-3 = narrative, 4 = 80-word credits.
			seedWordsManifest(t, work, style, []int{3, 3000, 3000, 80})
			class, err := classifyBookEdges(work)
			if err != nil {
				t.Fatalf("classifyBookEdges: %v", err)
			}
			if class.LogicalCount != 2 { // n-2
				t.Errorf("LogicalCount = %d, want 2", class.LogicalCount)
			}
			if !reflect.DeepEqual(class.ExcludedLeading, []int{1}) || !reflect.DeepEqual(class.ExcludedTrailing, []int{4}) {
				t.Errorf("excluded leading=%v trailing=%v, want [1] and [4]", class.ExcludedLeading, class.ExcludedTrailing)
			}
			for _, sub := range []string{"Audio files 1 and 4 are non-narrative", "1-2 as spoken"} {
				if !strings.Contains(class.EdgeNote, sub) {
					t.Errorf("EdgeNote %q missing %q", class.EdgeNote, sub)
				}
			}
		})
	}
}

func TestClassifyManifestEdgesNormalBookNoExclusions(t *testing.T) {
	// A normal book of either style (every chapter well above the threshold) is a no-op.
	for _, style := range []string{audio.StyleFiles, audio.StyleMarkers} {
		t.Run(style, func(t *testing.T) {
			work := t.TempDir()
			seedWordsManifest(t, work, style, []int{2500, 3000, 2800, 3100})
			class, err := classifyBookEdges(work)
			if err != nil {
				t.Fatalf("classifyBookEdges: %v", err)
			}
			if class.LogicalCount != 4 || class.EdgeNote != "" || class.ExcludedLeading != nil || class.ExcludedTrailing != nil {
				t.Errorf("class = %+v, want logical 4 with no exclusions and an empty note", class)
			}
		})
	}
}

// TestSynthesisPromptCarriesEdgeNote is the stage-level assertion: an edge-file book's
// synthesis prompt carries the LOGICAL chapter count and the EdgeNote, while a normal
// book's prompt carries the raw count and NO note (byte-identical to before).
func TestSynthesisPromptCarriesEdgeNote(t *testing.T) {
	t.Run("edge-file book injects the note and the logical count", func(t *testing.T) {
		work := t.TempDir()
		// 5 files: intro + 3 narrative + credits -> logical 3, excludes 1 and 5.
		seedWordsManifest(t, work, audio.StyleFiles, []int{3, 2000, 2000, 2000, 45})
		seedFacts(t, work)

		fake := newFakeRunner()
		fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
			writeOutSidecars(t, req, "book")
			return agent.Result{}, nil
		}
		exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
		if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Synthesizing, scheduler.StageReport{}); err != nil {
			t.Fatalf("synthesize: %v", err)
		}
		prompt := fake.lastPrompt(string(state.Synthesizing))
		for _, sub := range []string{"a work of 3 logical chapters", "Note on this book's structure", "non-narrative", "1-3 as spoken"} {
			if !strings.Contains(prompt, sub) {
				t.Errorf("synthesis prompt missing %q; got:\n%s", sub, prompt)
			}
		}
	})

	t.Run("normal book carries the raw count and no note", func(t *testing.T) {
		work := t.TempDir()
		seedWordsManifest(t, work, audio.StyleFiles, []int{2000, 2000, 3000})
		seedFacts(t, work)

		fake := newFakeRunner()
		fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
			writeOutSidecars(t, req, "book")
			return agent.Result{}, nil
		}
		exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
		if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Synthesizing, scheduler.StageReport{}); err != nil {
			t.Fatalf("synthesize: %v", err)
		}
		prompt := fake.lastPrompt(string(state.Synthesizing))
		if !strings.Contains(prompt, "a work of 3 logical chapters") {
			t.Errorf("normal-book prompt missing the raw count; got:\n%s", prompt)
		}
		if strings.Contains(prompt, "Note on this book's structure") || strings.Contains(prompt, "non-narrative") {
			t.Errorf("normal-book prompt should carry no edge note; got:\n%s", prompt)
		}
	})
}

// TestAuditPromptCarriesEdgeNote asserts the auditor (the deadlock site) is told the
// logical count and the trailing files are not story chapters. It uses a MARKERS
// manifest - the live incident shape (a single-file m4b with bare-number contiguous
// markers, an intro + credits) that the earlier StyleFiles-only gate missed.
func TestAuditPromptCarriesEdgeNote(t *testing.T) {
	work := t.TempDir()
	seedWordsManifest(t, work, audio.StyleMarkers, []int{3, 2000, 2000, 2000, 45}) // logical 3
	seedFacts(t, work)
	seedWorkSidecars(t, work, baseChars("book"), baseRecaps("book"))
	writeJSON(t, filepath.Join(work, validationReportName), validationReport{Clean: true, Errors: []string{}})

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, auditReportName, AuditReport{Pass: true, Findings: []AuditFinding{}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withSidecarAgent(t.TempDir(), fake))
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.Auditing, scheduler.StageReport{}); err != nil {
		t.Fatalf("auditing: %v", err)
	}
	prompt := fake.lastPrompt(string(state.Auditing))
	for _, sub := range []string{"(3 logical chapters)", "Note on this book's structure", "not story chapters", "last logical story chapter (3)"} {
		if !strings.Contains(prompt, sub) {
			t.Errorf("audit prompt missing %q; got:\n%s", sub, prompt)
		}
	}
}

// inflameChapters models the live book that exposed the positional-numbering bug: an Audible
// intro (file 1), an UNNUMBERED Prologue (file 2), chapters 1-59 (files 3-61), an Epilogue
// (62), a bloopers reel (63) and closing credits (64). Counting positions made that 62
// logical chapters at offset 1, so every reveal and recap gate landed a chapter early and the
// audit rejected them as disclosing later material for round after round.
func inflameChapters() []edgeChapter {
	chs := []edgeChapter{
		{Chapter: 1, Words: 3, HasTranscript: true, Probed: true, DurationSec: 19.6, Opening: "This is Audible."},
		{Chapter: 2, Words: 1800, HasTranscript: true, Probed: true, DurationSec: 677.9, Opening: "Prologue Hello A musical voice called over as Joe peered around from the fluffy chair"},
	}
	for file := 3; file <= 61; file++ {
		ch := edgeChapter{Chapter: file, Words: 2500, HasTranscript: true, DurationSec: 900}
		// Only the probe window is read for a large book; the interior stays an unprobed sentinel.
		if file <= edgeProbeDepth || file > 64-edgeProbeDepth {
			ch.Probed = true
			ch.Opening = fmt.Sprintf("%d. The chapter begins here with some narration.", file-2)
		}
		chs = append(chs, ch)
	}
	return append(chs,
		edgeChapter{Chapter: 62, Words: 1900, HasTranscript: true, Probed: true, DurationSec: 712, Opening: "Epilogue Getting out of Grandma's shoe was simple after the"},
		edgeChapter{Chapter: 63, Words: 800, HasTranscript: true, Probed: true, DurationSec: 309, Opening: "Bloopers! He couldn't get out more than a painted grunt."},
		edgeChapter{Chapter: 64, Words: 40, HasTranscript: true, Probed: true, DurationSec: 27.6, Opening: "This has been a production of Audible Studios."},
	)
}

func TestClassifyEdgeChaptersUsesNarratedNumbering(t *testing.T) {
	got := classifyEdgeChapters(inflameChapters())
	if got.ChapterOffset != 2 {
		t.Errorf("offset = %d, want 2 (file 3 announces chapter 1)", got.ChapterOffset)
	}
	if got.LogicalCount != 59 {
		t.Errorf("logical count = %d, want 59 (files 3-61 are chapters 1-59)", got.LogicalCount)
	}
	if len(got.FrontMatter) != 1 || got.FrontMatter[0] != 2 {
		t.Errorf("front matter = %v, want [2] (the unnumbered Prologue)", got.FrontMatter)
	}
	if len(got.EndMatter) != 2 || got.EndMatter[0] != 62 || got.EndMatter[1] != 63 {
		t.Errorf("end matter = %v, want [62 63] (Epilogue and bloopers)", got.EndMatter)
	}
	if len(got.ExcludedLeading) != 1 || got.ExcludedLeading[0] != 1 {
		t.Errorf("excluded leading = %v, want [1]", got.ExcludedLeading)
	}
	if len(got.ExcludedTrailing) != 1 || got.ExcludedTrailing[0] != 64 {
		t.Errorf("excluded trailing = %v, want [64]", got.ExcludedTrailing)
	}
	// The mapping the fact-pass assembly is told must be the derived one; "N-1" was the bug.
	if !strings.Contains(got.AssembleNote, "spoken story chapter N-2") {
		t.Errorf("assemble note must carry the derived offset: %q", got.AssembleNote)
	}
	if !strings.Contains(got.EdgeNote, "1-59") || !strings.Contains(got.EdgeNote, "position 0") {
		t.Errorf("edge note must carry the numbered range and the front-matter rule: %q", got.EdgeNote)
	}
}

// With no announcements to read, the classifier keeps its previous positional behaviour
// exactly - the overwhelming majority of books, whose prompts must render as before.
func TestClassifyEdgeChaptersFallsBackWithoutAnnouncements(t *testing.T) {
	chs := []edgeChapter{{Chapter: 1, Words: 3, HasTranscript: true, Probed: true, DurationSec: 2, Opening: "This is Audible."}}
	for file := 2; file <= 20; file++ {
		chs = append(chs, edgeChapter{Chapter: file, Words: 2500, HasTranscript: true, Probed: true, DurationSec: 900,
			Opening: "The morning came slowly over the hills and Joe considered his options."})
	}
	got := classifyEdgeChapters(chs)
	if got.ChapterOffset != 1 || got.LogicalCount != 19 {
		t.Errorf("offset/logical = %d/%d, want 1/19 (positional fallback)", got.ChapterOffset, got.LogicalCount)
	}
	if len(got.FrontMatter) != 0 || len(got.EndMatter) != 0 {
		t.Errorf("fallback must claim no unnumbered sections: front=%v end=%v", got.FrontMatter, got.EndMatter)
	}
}

// A single stray parse must not shift a book: the run requirement needs consecutive files to
// announce consecutive numbers.
func TestClassifyEdgeChaptersIgnoresIsolatedNumberOpening(t *testing.T) {
	chs := []edgeChapter{{Chapter: 1, Words: 3, HasTranscript: true, Probed: true, DurationSec: 2, Opening: "This is Audible."}}
	for file := 2; file <= 20; file++ {
		opening := "The morning came slowly over the hills and Joe considered his options."
		if file == 5 {
			opening = "Two hundred. The count was grim." // parses to 200 -> offset -195, but stands alone
		}
		chs = append(chs, edgeChapter{Chapter: file, Words: 2500, HasTranscript: true, Probed: true, DurationSec: 900, Opening: opening})
	}
	got := classifyEdgeChapters(chs)
	if got.ChapterOffset != 1 || got.LogicalCount != 19 {
		t.Errorf("one stray announcement shifted the book: offset/logical = %d/%d, want 1/19", got.ChapterOffset, got.LogicalCount)
	}
}

// Consecutive files repeating the SAME number (a stuck transcript) are not a numbering.
func TestClassifyEdgeChaptersRejectsRepeatedSameNumber(t *testing.T) {
	var chs []edgeChapter
	for file := 1; file <= 12; file++ {
		chs = append(chs, edgeChapter{Chapter: file, Words: 2500, HasTranscript: true, Probed: true, DurationSec: 900,
			Opening: "Seven. The same opening every time."})
	}
	got := classifyEdgeChapters(chs)
	if got.ChapterOffset != 0 || got.LogicalCount != 12 {
		t.Errorf("a stuck repeated announcement was treated as a numbering: offset/logical = %d/%d, want 0/12", got.ChapterOffset, got.LogicalCount)
	}
}
