package ebook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kodestar/audiosilo-meta/pkg/extract"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
)

// Doc is one emitted text section plus the verdict this package reached about it.
type Doc struct {
	Index  int    `json:"index"`
	File   string `json:"file"`
	Spine  int    `json:"spine"`
	Anchor string `json:"anchor,omitempty"`
	Label  string `json:"label,omitempty"`
	Words  int    `json:"words"`
	// Chapter is the logical chapter this section belongs to (0 = none).
	Chapter int `json:"chapter,omitempty"`
	// Source records HOW Chapter was decided, so a human reading the manifest can
	// tell a number the book stated from one this package assigned.
	Source Source `json:"source,omitempty"`
	// Quarantine is empty when the section is part of the story, else why it is not.
	Quarantine string `json:"quarantine,omitempty"`
	// Head is the section's opening few words, populated by PopulateHeads.
	//
	// It is the ONE judgement a toc label cannot support: telling "Acknowledgements"
	// from chapter one of a DIFFERENT novel when neither is labelled. It is capped at
	// HeadWords - far below any n-gram shingle - so the mapping agent can recognize
	// what a section is without the book's prose reaching it.
	Head string `json:"head,omitempty"`
}

// Source is how a section's chapter number was decided.
type Source string

const (
	// SourceStrict: the toc label stated the number outright ("Chapter 12").
	SourceStrict Source = "strict"
	// SourceLoose: the label's number was ambiguous per-label but the book's whole
	// run of labels came out contiguous, which is what confirms the reading.
	SourceLoose Source = "loose"
	// SourceOrdinal: the labels carry no numbers, so the story sections were
	// numbered 1..N in spine order - what a human does for a titled-only book, and
	// correct under the position model, whose chapter is the logical work chapter
	// rather than anything printed.
	SourceOrdinal Source = "ordinal"
	// SourceContinuation: an UNLABELED section between two numbered ones, folded into
	// the preceding chapter (a chapter split across several spine files).
	SourceContinuation Source = "continuation"
	// SourceInterstitial: a LABELED but unnumbered section between two numbered ones -
	// an Interlude, a letter, a "Then"/"Now" divider. Folded into the FOLLOWING
	// chapter, because the reader reaches it after finishing the one before.
	SourceInterstitial Source = "interstitial"
	// SourceAgent: the chapter_mapping agent decided this section's number, because
	// the labels alone did not yield a usable run.
	SourceAgent Source = "agent"
)

// Quarantine reasons.
const (
	quarantineFront    = "front matter"
	quarantineBack     = "back matter"
	quarantineExcerpt  = "back matter: looks like an excerpt of another book"
	quarantineNoStory  = "no story chapters were identified"
	quarantineFrontVoc = "front matter (named in the toc)"
)

// Chapter is one logical story chapter: the number published in the sidecars.
type Chapter struct {
	Chapter int      `json:"chapter"`
	Title   string   `json:"title,omitempty"`
	Files   []string `json:"files"`
	Words   int      `json:"words"`
}

// Universe is the extracting stage's verdict over a split epub.
type Universe struct {
	Chapters   []Chapter `json:"chapters"`
	Docs       []Doc     `json:"docs"`
	Contiguous bool      `json:"contiguous"`
	// Suspected marks a quarantined trailing run that looks like an excerpt of a
	// DIFFERENT book rather than ordinary back matter. Already excluded either way;
	// the flag drives a loud stage note, because letting another book's opening
	// chapter reach the fact pass is the trap EXTRACTION.md calls out by name.
	Suspected bool `json:"suspected_excerpt,omitempty"`
	// Labeled counts sections carrying a toc label at all. It separates "the parser
	// does not know this dialect" (labels present, none numbered - an agent can map
	// it) from "there is no toc to work with" (a park).
	Labeled int      `json:"labeled"`
	Words   int      `json:"words"`
	Notes   []string `json:"notes,omitempty"`
}

// excerptWordFloor is the size past which a quarantined trailing section stops
// looking like a dedication and starts looking like a chapter of another book.
const excerptWordFloor = 2000

// minChapters is the shortest run treated as a real chapter universe, matching the
// floor extract.Contiguous applies to a label-derived one.
const minChapters = 3

// excerptCues are label fragments that announce a promo excerpt. Matched
// case-insensitively as substrings.
var excerptCues = []string{
	"excerpt", "sneak peek", "sneak preview", "preview of", "read on for",
	"teaser", "coming soon", "bonus chapter", "also by", "keep reading",
}

// boilerplateCues name back matter that is long but obviously not a story: a
// Project Gutenberg licence runs to ~2,900 words, comfortably past excerptWordFloor.
//
// They exist to keep the excerpt flag RARE. Length alone flagged 25 of 33 books in
// the validation corpus, almost all of them this licence - and a warning that fires
// on three books in four is one a contributor learns to click past, which is how
// the real case would slip by.
var boilerplateCues = []string{
	"project gutenberg", "licence", "license", "footnote", "endnote",
	"appendix", "glossary", "bibliography", "transcriber", "index",
	"about the publisher", "colophon", "copyright",
}

// frontMatterLabels are the toc labels that name apparatus rather than story. Used
// only to EXPLAIN a quarantine, never to decide one: the decision is positional
// (before the first numbered chapter, after the last), so an unrecognized label at
// the edges is quarantined just the same.
// apparatusCues name apparatus that an exact label match cannot catch, because the
// label carries the book's own words: "Books by Rick Riordan", "List of
// Illustrations", "About the Author and Illustrator". Matched case-insensitively as
// substrings, and only at the EDGES of a titled-only book.
//
// They matter far more than they look. A numbered book excludes its apparatus by
// position, off numbers the book itself states; a titled-only book has nothing but
// these labels to go on, so an untrimmed "Copyright" becomes chapter 1 and shifts
// every real chapter - which is a wrong spoiler position on every reveal in the book.
//
// Prologue, Epilogue, Interlude and Part are deliberately absent: those are story.
var apparatusCues = []string{
	"about the author", "about the illustrator", "about the publisher",
	"books by", "also by", "other books", "more by",
	"list of illustrations", "list of maps", "list of characters",
	"table of contents", "title page", "half title",
	"copyright", "dedication", "acknowledg", "colophon", "imprint",
	"afterword", "foreword", "preface", "introduction",
	"newsletter", "reading group", "discussion questions",
	// Public-domain apparatus, which every Gutenberg-derived epub ends with and which
	// is long enough (~2,900 words) to read as a chapter on any size test.
	"project gutenberg", "license", "licence", "transcriber",
	"appendix", "glossary", "bibliography",
}

var frontMatterLabels = map[string]bool{
	"cover": true, "title page": true, "titlepage": true, "contents": true,
	"table of contents": true, "copyright": true, "dedication": true,
	"acknowledgements": true, "acknowledgments": true, "about the author": true,
	"by the same author": true, "also by the author": true, "epigraph": true,
	"half title": true, "imprint": true, "colophon": true, "credits": true,
}

// BuildUniverse derives the logical chapter universe from a split epub's manifest.
//
// The order of business is: read what the labels state, decide whether the book's
// numbering is trustworthy as a whole, quarantine everything outside the story, and
// only then - if there were no numbers at all - fall back to ordinals.
//
// It never returns a partial numbering. Either the chapters form a contiguous run
// (Contiguous true, safe to publish positions against) or they do not, and the
// caller routes the book to the chapter-mapping agent.
func BuildUniverse(m *extract.Manifest) Universe {
	u := Universe{}
	if m == nil || len(m.Docs) == 0 {
		return u
	}

	docs := make([]Doc, len(m.Docs))
	strictNums := make([]int, 0, len(m.Docs))
	looseNums := make([]int, 0, len(m.Docs))
	for i, d := range m.Docs {
		docs[i] = Doc{
			Index: d.Index, File: d.File, Spine: d.Spine,
			Anchor: d.Anchor, Label: d.Label, Words: d.Words,
		}
		u.Words += d.Words
		if strings.TrimSpace(d.Label) != "" {
			u.Labeled++
		}
		switch n, _, conf := extract.ChapterFromLabel(d.Label); conf {
		case extract.Strict:
			docs[i].Chapter, docs[i].Source = n, SourceStrict
			strictNums = append(strictNums, n)
			looseNums = append(looseNums, n)
		case extract.Loose:
			docs[i].Chapter, docs[i].Source = n, SourceLoose
			looseNums = append(looseNums, n)
		}
	}

	// Take the ambiguous readings only when they EXTEND the run and the whole book
	// then agrees - that agreement over a strictly larger set is the evidence a single
	// ambiguous label cannot supply. Testing "loose is longer" before "strict is
	// contiguous" is load-bearing: looseNums is a superset of strictNums, so both can
	// be contiguous at once (a toc that drops its separator on the last two entries
	// gives strict 1,2,3 and loose 1..5). Preferring strict there would publish the
	// book's final chapters as back matter, which is silent and unrecoverable.
	//
	// When the two are the same length they are the same set, so an equal-length loose
	// run needs no case of its own.
	switch {
	case extract.Contiguous(looseNums) && len(looseNums) > len(strictNums):
		u.Contiguous = true
		u.Notes = append(u.Notes, fmt.Sprintf(
			"chapter numbers came from labels that are ambiguous on their own (e.g. a leading Roman numeral); "+
				"accepted because all %d form a contiguous run", len(looseNums)))
	case extract.Contiguous(strictNums):
		dropLoose(docs)
		u.Contiguous = true
	default:
		dropLoose(docs)
	}

	// A book that numbers its prologue "Chapter 0" is 0-based, and extract.Contiguous
	// accepts such a run. The position model is 1-based (0 means prior-book
	// knowledge), and everything downstream treats Chapter == 0 as "no chapter" - so
	// without this the prologue is quarantined as front matter, losing its text AND
	// leaving every later chapter one position below where the reader is.
	if u.Contiguous && shiftZeroBased(docs) {
		u.Notes = append(u.Notes, "the book numbers its chapters from 0, so every number was shifted up by one "+
			"to match the 1-based position model")
	}

	// With no numbers anywhere, number the story sections in spine order. Only safe
	// once the edges are quarantined, so it happens after the split below.
	numbered := anyNumbered(docs)
	first, last := storySpan(docs)
	if !numbered {
		first, last = titledStorySpan(docs)
	}

	u.Suspected = quarantineEdges(docs, first, last)
	if u.Suspected {
		u.Notes = append(u.Notes, "a trailing section looks like an excerpt of a different book and was excluded; "+
			"confirm before contributing, since another book's text must never reach the fact pass")
	}

	if !numbered {
		assignOrdinals(docs, first, last)
	}
	foldContinuations(docs, first, last)
	u.Docs = docs
	u.Chapters = CollectChapters(docs, nil)
	// minChapters is the same floor extract.Contiguous applies to a label-derived
	// run, restated once for the ordinal path rather than in both branches.
	if len(u.Chapters) < minChapters {
		u.Contiguous = false
	} else if !numbered {
		if outlier, median := ordinalSizeOutlier(u.Chapters); outlier > 0 {
			u.Notes = append(u.Notes, fmt.Sprintf(
				"the toc states no chapter numbers and section %d is %d words against a median of %d, "+
					"so it is a divider or apparatus rather than a chapter; numbering it would shift every "+
					"later position, so the mapping agent decides this book",
				outlier, u.Chapters[outlier-1].Words, median))
		} else {
			u.Contiguous = true
			u.Notes = append(u.Notes, fmt.Sprintf(
				"the toc states no chapter numbers, so the %d story sections were numbered in reading order",
				len(u.Chapters)))
		}
	}
	return u
}

// ordinalOutlierRatio is how far below the median a titled-only section may sit and
// still be believable as a chapter of the same book.
const ordinalOutlierRatio = 10

// ordinalSizeOutlier returns the 1-based position of the first ordinal chapter far
// too small to be one, with the book's median chapter size, or 0 when the run is even.
//
// The ordinal path has nothing but labels to go on, so a divider page the toc named
// ("Zeus", 74 words, sitting between 6,000-word myths) or apparatus the edge trim did
// not recognize becomes a chapter of its own and shifts every position after it. Size
// is the one signal the labels cannot give: real chapters of one book are the same
// order of magnitude.
//
// Refusing is cheap and correct - the book routes to the chapter-mapping agent, which
// has each section's opening words and can say what it is. Guessing is what costs,
// because the shifted position is published and nothing downstream re-derives it.
func ordinalSizeOutlier(chs []Chapter) (position, median int) {
	if len(chs) < minChapters {
		return 0, 0
	}
	words := make([]int, 0, len(chs))
	for _, c := range chs {
		words = append(words, c.Words)
	}
	sort.Ints(words)
	median = words[len(words)/2]
	if median <= 0 {
		return 0, 0
	}
	for i, c := range chs {
		if c.Words*ordinalOutlierRatio < median {
			return i + 1, median
		}
	}
	return 0, median
}

// dropLoose clears the ambiguous readings, so a manifest never records a number the
// book's own labels did not justify as a whole.
func dropLoose(docs []Doc) {
	for i := range docs {
		if docs[i].Source == SourceLoose {
			docs[i].Chapter, docs[i].Source = 0, ""
		}
	}
}

// shiftZeroBased renumbers a 0-based run to 1-based, reporting whether it did.
//
// A section carrying Source but Chapter 0 is the tell: an unnumbered section has no
// Source at all, and dropLoose has already cleared the readings that were not
// accepted, so only labels the book itself numbered are shifted.
func shiftZeroBased(docs []Doc) bool {
	zeroBased := false
	for _, d := range docs {
		if d.Chapter == 0 && d.Source != "" {
			zeroBased = true
			break
		}
	}
	if !zeroBased {
		return false
	}
	for i := range docs {
		if docs[i].Source != "" {
			docs[i].Chapter++
		}
	}
	return true
}

func anyNumbered(docs []Doc) bool {
	for _, d := range docs {
		if d.Chapter > 0 {
			return true
		}
	}
	return false
}

// storySpan returns the index of the first and last section of the story.
//
// It starts at the last NUMBERED section, then extends over any unlabeled sections
// carved out of that SAME spine document - the closing pages of a chapter the toc
// anchored mid-file, which would otherwise fall outside the span and be thrown away
// as back matter (silently, since the tail of a chapter is usually too short to
// trip the excerpt flag).
//
// It deliberately does not extend into a NEW spine file. A promo excerpt of the next
// book is also unlabeled and also trails the last chapter, so admitting one on
// position alone is exactly the failure quarantineEdges exists to prevent; sharing
// the last chapter's spine document is what proves a section is its continuation
// rather than something appended after it.
func storySpan(docs []Doc) (first, last int) {
	first, last = -1, -1
	for i, d := range docs {
		if d.Chapter > 0 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return -1, -1
	}
	for last+1 < len(docs) && isSpineContinuation(docs[last], docs[last+1]) {
		last++
	}
	return first, last
}

// isSpineContinuation reports whether next is an unlabeled remainder of the same
// spine document as prev.
func isSpineContinuation(prev, next Doc) bool {
	return next.Chapter == 0 && next.Quarantine == "" &&
		strings.TrimSpace(next.Label) == "" && next.Spine == prev.Spine
}

// titledStorySpan guesses the story span for a book whose toc carries labels but no
// numbers, by trimming the runs of recognized apparatus labels at each end. It is
// deliberately conservative: an unrecognized label counts as story, so the span only
// shrinks for labels we are confident about.
func titledStorySpan(docs []Doc) (first, last int) {
	first, last = 0, len(docs)-1
	for first <= last && isApparatus(docs[first]) {
		first++
	}
	for last >= first && isApparatus(docs[last]) {
		last--
	}
	if first > last {
		return -1, -1
	}
	return first, last
}

func isApparatus(d Doc) bool {
	label := strings.ToLower(strings.Trim(strings.TrimSpace(d.Label), " .:"))
	if label == "" {
		return true // an unlabeled edge section is apparatus by position
	}
	if frontMatterLabels[label] {
		return true
	}
	return containsAny(label, apparatusCues) || containsAny(label, excerptCues)
}

// looksLikeExcerpt reports whether a quarantined trailing section is plausibly a
// chapter of a DIFFERENT book rather than ordinary back matter.
//
// Two signals, and the asymmetry is deliberate. A label that announces an excerpt
// ("read on for a preview of ...") says so outright, so it flags at any length.
// Otherwise the section has to be long AND not recognizable boilerplate: a promo
// excerpt is a full chapter and, per EXTRACTION.md's Killing Floor case, typically
// carries no toc label at all, whereas a licence or an appendix names itself.
//
// Either way the section is already excluded. This only decides whether to raise
// the flag that asks a human to look.
func looksLikeExcerpt(d Doc) bool {
	if containsAny(d.Label, excerptCues) {
		return true
	}
	return d.Words >= excerptWordFloor && !containsAny(d.Label, boilerplateCues)
}

// containsAny reports whether label contains any of cues, case-insensitively. One
// scanner for both cue lists, so the two cannot drift apart (a TrimSpace added to
// one and not the other would silently change which sections get flagged).
func containsAny(label string, cues []string) bool {
	low := strings.ToLower(label)
	for _, cue := range cues {
		if strings.Contains(low, cue) {
			return true
		}
	}
	return false
}

// quarantineEdges excludes everything outside the story span.
//
// The rule is POSITIONAL and length-independent, which is the whole point. The
// audio path can classify an edge chapter by size because an opening credit is
// short; a trailing promo excerpt is a full chapter of a DIFFERENT book, thousands
// of words long, so any word-count rule waves it straight through and its content
// reaches the fact pass. Position is what distinguishes it.
func quarantineEdges(docs []Doc, first, last int) (suspected bool) {
	if first < 0 {
		for i := range docs {
			docs[i].Quarantine = quarantineNoStory
		}
		return false
	}
	for i := range docs {
		if i >= first && i <= last {
			continue
		}
		docs[i].Chapter, docs[i].Source = 0, ""
		switch {
		case i < first:
			docs[i].Quarantine = quarantineFront
			if frontMatterLabels[strings.ToLower(strings.TrimSpace(docs[i].Label))] {
				docs[i].Quarantine = quarantineFrontVoc
			}
		case looksLikeExcerpt(docs[i]):
			docs[i].Quarantine = quarantineExcerpt
			suspected = true
		default:
			docs[i].Quarantine = quarantineBack
		}
	}
	return suspected
}

// assignOrdinals numbers the story sections the toc NAMED 1..N in reading order.
//
// Only a labeled section starts a chapter. An unlabeled one is a chapter split
// across several files, and numbering it too would insert a phantom chapter that
// shifts every later position by one - the identical physical book is folded
// correctly whenever its toc happens to number its entries, so numbering here
// instead would make correctness depend on that accident. foldContinuations
// attaches them afterwards.
//
// titledStorySpan trims unlabeled edges, so docs[first] always carries a label and
// the run can never start with an unattachable section.
func assignOrdinals(docs []Doc, first, last int) {
	if first < 0 {
		return
	}
	n := 0
	for i := first; i <= last; i++ {
		if strings.TrimSpace(docs[i].Label) == "" {
			continue
		}
		n++
		docs[i].Chapter, docs[i].Source = n, SourceOrdinal
	}
}

// foldContinuations attaches every unnumbered story section to a numbered one, in
// the direction that cannot date a fact to a chapter the reader has already passed.
//
// An UNLABELED section is a continuation - a chapter split across several files,
// whose later files carry no toc entry of their own - and belongs to the chapter
// before it.
//
// A LABELED but unnumbered section is an interstitial the toc named: an Interlude, a
// letter, a "Then"/"Now" divider between numbered chapters. It is its own piece of
// story sitting AFTER the chapter before it, so folding it backward would attribute
// its facts to a chapter the reader finishes BEFORE reading it, firing every reveal
// in it one section early. It folds FORWARD, into the chapter it precedes.
//
// The forward pass runs first and anchors only on sections that were already
// numbered, so a continuation folded by the backward pass can never act as an anchor
// and drag an interstitial back the way it came.
func foldContinuations(docs []Doc, first, last int) {
	if first < 0 {
		return
	}
	next := 0
	for i := last; i >= first; i-- {
		switch {
		case docs[i].Quarantine != "":
		case docs[i].Chapter > 0:
			next = docs[i].Chapter
		case strings.TrimSpace(docs[i].Label) != "" && next > 0:
			docs[i].Chapter, docs[i].Source = next, SourceInterstitial
		}
	}
	current := 0
	for i := first; i <= last; i++ {
		if docs[i].Quarantine != "" {
			continue
		}
		if docs[i].Chapter > 0 {
			current = docs[i].Chapter
			continue
		}
		if current > 0 {
			docs[i].Chapter, docs[i].Source = current, SourceContinuation
		}
	}
}

// CollectChapters folds sections into logical chapters in chapter order. titles
// overrides a chapter's title when the caller has a better one than the labels do
// (the mapping agent supplies them); a nil map keeps the label-derived titles.
func CollectChapters(docs []Doc, titles map[int]string) []Chapter {
	byNum := map[int]*Chapter{}
	var order []int
	for _, d := range docs {
		if d.Chapter <= 0 || d.Quarantine != "" {
			continue
		}
		c, ok := byNum[d.Chapter]
		if !ok {
			c = &Chapter{Chapter: d.Chapter}
			byNum[d.Chapter] = c
			order = append(order, d.Chapter)
		}
		c.Files = append(c.Files, d.File)
		c.Words += d.Words
		// Only a section that OPENS a chapter can name it. A folded one - in either
		// direction - carries its own label ("Interlude"), which is not the title of
		// the chapter it was attached to.
		if c.Title == "" && d.Source != SourceContinuation && d.Source != SourceInterstitial {
			if _, title, _ := extract.ChapterFromLabel(d.Label); title != "" {
				c.Title = title
			} else if d.Source == SourceOrdinal {
				c.Title = strings.TrimSpace(d.Label)
			}
		}
	}
	sort.Ints(order)
	out := make([]Chapter, 0, len(order))
	for _, n := range order {
		c := *byNum[n]
		if t, ok := titles[n]; ok && t != "" {
			c.Title = t
		}
		out = append(out, c)
	}
	return out
}

// ReparseManifest re-derives the chapter universe from a persisted extract
// manifest, using the CURRENT label vocabulary and without re-reading the epub.
//
// It is the free upgrade path: a book parked because an older parser could not
// read its numbering is recovered by a later one at no cost, the same recovery
// the audio path gets from ReparseMarkerManifest. Section provenance (label,
// words, spine, head) is preserved; only the verdicts are recomputed.
func ReparseManifest(workDir string) (Universe, error) {
	prev, err := ReadManifest(workDir)
	if err != nil {
		return Universe{}, err
	}
	return ReparseUniverse(prev), nil
}

// ReparseUniverse re-derives a chapter universe from an already-loaded one, so a
// caller holding the draft does not read and parse the same manifest twice.
func ReparseUniverse(prev Universe) Universe {
	docs := make([]extract.DocEntry, 0, len(prev.Docs))
	for _, d := range prev.Docs {
		docs = append(docs, extract.DocEntry{
			Index: d.Index, Spine: d.Spine, File: d.File,
			Anchor: d.Anchor, Label: d.Label, Words: d.Words,
		})
	}
	u := BuildUniverse(&extract.Manifest{Docs: docs})
	// Carry the recorded heads across: they were read from the split text, which the
	// reparse does not touch.
	heads := make(map[string]string, len(prev.Docs))
	for _, d := range prev.Docs {
		heads[d.File] = d.Head
	}
	for i := range u.Docs {
		u.Docs[i].Head = heads[u.Docs[i].File]
	}
	return u
}

// WriteChapterText materializes one text file per LOGICAL chapter under TextDir,
// concatenating the sections that make up each chapter in reading order.
//
// The filenames are chNNN.txt, matching the audio path's convention, so the
// authoring tail's staging loop is the same code for both kinds. Chapter N's file
// IS spoken chapter N: unlike audio, where m4b track numbers are fixed and a
// file-to-chapter offset has to be carried through the prompts, extracting owns
// these names, so the ebook path has no renumbering boundary at all.
func WriteChapterText(workDir, splitDir string, u Universe) error {
	dir := filepath.Join(workDir, TextDir)
	// Replace the layer rather than merging into it. A re-derivation that yields
	// FEWER chapters (a second mapping round quarantining a section the first kept)
	// would otherwise leave the old tail behind, and ngramCheck compares the whole
	// directory - so prose belonging to a chapter that no longer exists would be
	// reported as near-verbatim overlap the fix loop can never resolve.
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, c := range u.Chapters {
		var b strings.Builder
		for i, f := range c.Files {
			raw, err := os.ReadFile(filepath.Join(splitDir, filepath.Base(f))) //nolint:gosec // both halves derive from the work dir
			if err != nil {
				return fmt.Errorf("chapter %d: read %s: %w", c.Chapter, f, err)
			}
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.Write(raw)
		}
		text := strings.TrimSpace(b.String()) + "\n"
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, ChapterFileName(c.Chapter)), []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// WriteManifest records the extract stage's audit trail: every emitted section with
// its label, size and chapter verdict. It is the ebook counterpart of probe.json -
// what the chapter-mapping agent is given, and what a human reads to see why a book
// parked.
func WriteManifest(workDir string, u Universe) error {
	out, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, ManifestName), append(out, '\n'), 0o644)
}

// ReadManifest loads a previously written universe, so a resumed or re-entered
// stage can re-derive from the recorded sections without re-splitting the epub.
func ReadManifest(workDir string) (Universe, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, ManifestName)) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return Universe{}, err
	}
	var u Universe
	if err := json.Unmarshal(raw, &u); err != nil {
		return Universe{}, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	return u, nil
}

// HeadWords is how much of a section's opening PopulateHeads records. Deliberately
// tiny: enough to recognize front matter or another book's first page, far too
// little to be a source of prose.
const HeadWords = 40

// PopulateHeads fills each section's Head from the split text on disk. A section
// that cannot be read is left with an empty Head rather than failing the stage -
// the mapping agent simply has less to go on for that one.
func PopulateHeads(u *Universe, splitDir string) {
	buf := make([]byte, headReadBytes)
	for i := range u.Docs {
		u.Docs[i].Head = readHead(filepath.Join(splitDir, filepath.Base(u.Docs[i].File)), buf)
	}
}

// headReadBytes bounds how much of a section is read to find its opening words.
// HeadWords of prose is a few hundred bytes, so one page is a generous ceiling -
// and reading whole sections instead would pull a 400-section book fully into
// memory and split every word of it to keep 40.
const headReadBytes = 4096

// readHead returns a section's opening HeadWords, or "" when it cannot be read (a
// missing head costs the mapping agent some context; it is not worth failing over).
func readHead(path string, buf []byte) string {
	f, err := os.Open(path) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	n, err := f.Read(buf)
	if n == 0 && err != nil {
		return ""
	}
	// The byte cut lands mid-rune roughly two times in three for a 3-byte script, and
	// strings.Fields keeps the broken remainder (it decodes to RuneError, not to
	// whitespace), so drop invalid sequences before splitting.
	fields := strings.Fields(strings.ToValidUTF8(string(buf[:n]), ""))
	if len(fields) > HeadWords {
		fields = fields[:HeadWords]
	}
	head := strings.Join(fields, " ")
	// A script that does not space its words (Chinese, Japanese, Thai) yields a
	// handful of newline-separated fields for a whole page, so the word cap never
	// binds and the head becomes ~1300 characters of the book's prose. Cap the runes
	// too, or "far too little to be a source of prose" holds only for English.
	if r := []rune(head); len(r) > headRuneCap {
		head = strings.TrimSpace(string(r[:headRuneCap]))
	}
	return head
}

// headRuneCap bounds a head for a script the word cap cannot measure. Comfortably
// above HeadWords of English prose (~250 characters), far below any n-gram shingle.
const headRuneCap = 320
