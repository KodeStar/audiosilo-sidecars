package ebook

import (
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/extract"
)

// doc is a terse builder for a split-epub manifest entry.
func doc(i int, label string, words int) extract.DocEntry {
	return extract.DocEntry{Index: i, Spine: i, File: chapterStem(i) + ".txt", Label: label, Words: words}
}

func manifest(docs ...extract.DocEntry) *extract.Manifest {
	return &extract.Manifest{Docs: docs}
}

func chapterNums(u Universe) []int {
	out := make([]int, 0, len(u.Chapters))
	for _, c := range u.Chapters {
		out = append(out, c.Chapter)
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuildUniverseNumberedChapters(t *testing.T) {
	u := BuildUniverse(manifest(
		doc(1, "Cover", 5),
		doc(2, "Title Page", 10),
		doc(3, "Chapter 1: The Start", 3000),
		doc(4, "Chapter 2: The Middle", 3200),
		doc(5, "Chapter 3: The End", 2800),
		doc(6, "About the Author", 80),
	))
	if !u.Contiguous {
		t.Fatalf("Contiguous = false, want true (notes: %v)", u.Notes)
	}
	if got := chapterNums(u); !equalInts(got, []int{1, 2, 3}) {
		t.Errorf("chapters = %v, want [1 2 3]", got)
	}
	if u.Chapters[0].Title != "The Start" {
		t.Errorf("chapter 1 title = %q, want %q", u.Chapters[0].Title, "The Start")
	}
	if u.Words != 5+10+3000+3200+2800+80 {
		t.Errorf("Words = %d", u.Words)
	}
	// Front and back matter are excluded, not numbered.
	for _, d := range u.Docs {
		switch d.Index {
		case 1, 2, 6:
			if d.Quarantine == "" {
				t.Errorf("doc %d (%q) was not quarantined", d.Index, d.Label)
			}
		default:
			if d.Quarantine != "" {
				t.Errorf("doc %d (%q) was quarantined: %s", d.Index, d.Label, d.Quarantine)
			}
		}
	}
}

// TestBuildUniverseQuarantinesLongTrailingExcerpt is the Killing Floor / Die Trying
// case, and the reason quarantine is positional rather than size-based: a promo
// excerpt is a full chapter of ANOTHER book, so every word-count rule passes it
// through and its plot reaches the fact pass.
func TestBuildUniverseQuarantinesLongTrailingExcerpt(t *testing.T) {
	u := BuildUniverse(manifest(
		doc(1, "Chapter 1", 3000),
		doc(2, "Chapter 2", 3000),
		doc(3, "Chapter 3", 3000),
		doc(4, "", 4200), // the next book's opening chapter, unlabeled and LONG
	))
	if !u.Contiguous {
		t.Fatalf("Contiguous = false, want true")
	}
	if got := chapterNums(u); !equalInts(got, []int{1, 2, 3}) {
		t.Errorf("chapters = %v, want [1 2 3] - the excerpt must not become chapter 4", got)
	}
	last := u.Docs[3]
	if last.Quarantine != quarantineExcerpt {
		t.Errorf("trailing 4200-word section quarantine = %q, want %q", last.Quarantine, quarantineExcerpt)
	}
	if !u.Suspected {
		t.Error("Suspected = false; a long trailing section must raise the excerpt flag")
	}
}

func TestBuildUniverseExcerptCueOnAShortSection(t *testing.T) {
	u := BuildUniverse(manifest(
		doc(1, "Chapter 1", 3000),
		doc(2, "Chapter 2", 3000),
		doc(3, "Chapter 3", 3000),
		doc(4, "Read on for an excerpt from Die Trying", 300),
	))
	if !u.Suspected {
		t.Error("Suspected = false; an excerpt cue must raise the flag even on a short section")
	}
}

// TestBuildUniverseFoldsContinuations: a chapter split across several spine files
// has toc entries only on the first. The later files must join that chapter, not
// become chapters of their own.
func TestBuildUniverseFoldsContinuations(t *testing.T) {
	u := BuildUniverse(manifest(
		doc(1, "Chapter 1", 1500),
		doc(2, "", 1400), // continuation of chapter 1
		doc(3, "Chapter 2", 1600),
		doc(4, "Chapter 3", 1600),
	))
	if got := chapterNums(u); !equalInts(got, []int{1, 2, 3}) {
		t.Fatalf("chapters = %v, want [1 2 3]", got)
	}
	if len(u.Chapters[0].Files) != 2 {
		t.Errorf("chapter 1 files = %v, want both sections", u.Chapters[0].Files)
	}
	if u.Chapters[0].Words != 2900 {
		t.Errorf("chapter 1 words = %d, want 2900", u.Chapters[0].Words)
	}
}

// TestBuildUniverseOrdinalsForTitledOnlyToc: a short-story collection numbers
// nothing. Numbering the story sections in reading order is what a human does, and
// is correct under the position model, whose chapter is the logical work chapter
// rather than anything printed in the book.
func TestBuildUniverseOrdinalsForTitledOnlyToc(t *testing.T) {
	u := BuildUniverse(manifest(
		doc(1, "Cover", 5),
		doc(2, "Introduction", 400),
		doc(3, "Perseus Wants a Hug", 3000),
		doc(4, "Psyche Ninjas a Box of Beauty Cream", 3100),
		doc(5, "Phaethon Fails Driver's Ed", 2900),
		doc(6, "About the Author", 60),
	))
	if !u.Contiguous {
		t.Fatalf("Contiguous = false, want true (notes: %v)", u.Notes)
	}
	// "Introduction" is not in the apparatus vocabulary, so it stays story rather
	// than being silently dropped - the conservative direction.
	if got := chapterNums(u); !equalInts(got, []int{1, 2, 3, 4}) {
		t.Errorf("chapters = %v, want [1 2 3 4]", got)
	}
	if u.Docs[2].Source != SourceOrdinal {
		t.Errorf("source = %q, want %q", u.Docs[2].Source, SourceOrdinal)
	}
}

// TestBuildUniverseGappedNumbersAreNotContiguous: a Part-structured book restarts
// its chapter numbering in each part, so the flat run has duplicates. That must
// route to the mapping agent rather than publish overlapping positions.
func TestBuildUniverseGappedNumbersAreNotContiguous(t *testing.T) {
	u := BuildUniverse(manifest(
		doc(1, "Part I", 20),
		doc(2, "Chapter 1", 3000),
		doc(3, "Chapter 2", 3000),
		doc(4, "Part II", 20),
		doc(5, "Chapter 1", 3000),
		doc(6, "Chapter 2", 3000),
	))
	if u.Contiguous {
		t.Error("Contiguous = true for a part-restarting book; duplicate positions must not be published")
	}
}

// TestBuildUniverseLooseAcceptedOnlyWhenWholeRunAgrees covers the confidence split:
// "I An Irate Neighbor" is ambiguous alone, but a full contiguous run of such
// labels is the evidence that settles it.
func TestBuildUniverseLooseAcceptedOnlyWhenWholeRunAgrees(t *testing.T) {
	agreeing := BuildUniverse(manifest(
		doc(1, "I An Irate Neighbor", 3000),
		doc(2, "II Selling in Haste", 3000),
		doc(3, "III Mr. Harrison at Home", 3000),
		doc(4, "IV Different Opinions", 3000),
	))
	if !agreeing.Contiguous {
		t.Errorf("a contiguous run of Roman labels was rejected (notes: %v)", agreeing.Notes)
	}
	if got := chapterNums(agreeing); !equalInts(got, []int{1, 2, 3, 4}) {
		t.Errorf("chapters = %v, want [1 2 3 4]", got)
	}

	// One stray ambiguous label among prose must NOT invent a chapter.
	stray := BuildUniverse(manifest(
		doc(1, "I hope Mr. Bingley will like it", 3000),
		doc(2, "A Walk to Meryton", 3000),
		doc(3, "The Ball at Netherfield", 3000),
	))
	for _, d := range stray.Docs {
		if d.Source == SourceLoose {
			t.Errorf("doc %q kept an unconfirmed loose reading", d.Label)
		}
	}
}

func TestBuildUniverseEmptyAndNoStory(t *testing.T) {
	if u := BuildUniverse(nil); u.Contiguous || len(u.Chapters) != 0 {
		t.Errorf("BuildUniverse(nil) = %+v, want an empty non-contiguous universe", u)
	}
	// A single unlabeled blob: nothing to number, and nothing an agent can map from
	// labels either - the caller parks it.
	u := BuildUniverse(manifest(doc(1, "", 90000)))
	if u.Contiguous {
		t.Error("Contiguous = true for a single unlabeled document")
	}
	if u.Labeled != 0 {
		t.Errorf("Labeled = %d, want 0", u.Labeled)
	}
}

// TestBuildUniverseLabeledButUnnumberedIsMappable separates the two empty cases:
// labels present but none understood is a vocabulary gap an agent can close, not a
// book without a toc.
func TestBuildUniverseLabeledButUnnumberedIsMappable(t *testing.T) {
	u := BuildUniverse(manifest(
		doc(1, "Act I, Scene 1", 900),
		doc(2, "Act I, Scene 2", 900),
		doc(3, "Act II, Scene 1", 900),
	))
	if u.Labeled != 3 {
		t.Errorf("Labeled = %d, want 3 - the caller uses this to route to the agent rather than park", u.Labeled)
	}
}

// TestExcerptFlagIgnoresBoilerplate keeps the excerpt warning RARE enough to be
// worth reading. A Project Gutenberg licence is ~2,900 words, so a pure
// word-count rule flagged 25 of 33 books in the validation corpus - and a warning
// that fires on three books in four is one a contributor learns to click past,
// which is exactly how a genuine cross-book excerpt would slip through.
func TestExcerptFlagIgnoresBoilerplate(t *testing.T) {
	boilerplate := []string{
		"THE FULL PROJECT GUTENBERG™ LICENSE",
		"FOOTNOTES:",
		"Appendix A: Sources",
		"Transcriber's Note",
	}
	for _, label := range boilerplate {
		u := BuildUniverse(manifest(
			doc(1, "Chapter 1", 3000),
			doc(2, "Chapter 2", 3000),
			doc(3, "Chapter 3", 3000),
			doc(4, label, 2900),
		))
		if u.Suspected {
			t.Errorf("%q raised the excerpt flag; long apparatus is not an excerpt", label)
		}
		// Still excluded from the story - only the flag differs.
		if u.Docs[3].Quarantine == "" {
			t.Errorf("%q was not quarantined", label)
		}
		if got := chapterNums(u); !equalInts(got, []int{1, 2, 3}) {
			t.Errorf("%q: chapters = %v, want [1 2 3]", label, got)
		}
	}

	// The unlabeled long trailing section still flags: that is the Die Trying shape.
	u := BuildUniverse(manifest(
		doc(1, "Chapter 1", 3000),
		doc(2, "Chapter 2", 3000),
		doc(3, "Chapter 3", 3000),
		doc(4, "", 4200),
	))
	if !u.Suspected {
		t.Error("an unlabeled 4200-word trailing section must still raise the flag")
	}
}
