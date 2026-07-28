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
	// SourceContinuation: an unnumbered section BETWEEN two numbered ones, folded
	// into the preceding chapter (a chapter split across several spine files).
	SourceContinuation Source = "continuation"
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

	// Prefer the strict reading. Fall back to including loose ones only when the
	// book's labels then form a contiguous run - that whole-book agreement is the
	// evidence a single ambiguous label cannot supply.
	switch {
	case extract.Contiguous(strictNums):
		dropLoose(docs)
		u.Contiguous = true
	case extract.Contiguous(looseNums):
		u.Contiguous = true
		u.Notes = append(u.Notes, fmt.Sprintf(
			"chapter numbers came from labels that are ambiguous on their own (e.g. a leading Roman numeral); "+
				"accepted because all %d form a contiguous run", len(looseNums)))
	default:
		dropLoose(docs)
	}

	// With no numbers anywhere, number the story sections in spine order. Only safe
	// once the edges are quarantined, so it happens after the split below.
	numbered := anyNumbered(docs)
	first, last := storySpan(docs)
	if !numbered {
		first, last = titledStorySpan(docs)
	}

	quarantineEdges(docs, first, last, &u)

	if !numbered {
		assignOrdinals(docs, first, last)
		if n := countNumbered(docs); n >= 3 {
			u.Contiguous = true
			u.Notes = append(u.Notes, fmt.Sprintf(
				"the toc states no chapter numbers, so the %d story sections were numbered in reading order", n))
		}
	}

	foldContinuations(docs, first, last)
	u.Docs = docs
	u.Chapters = collectChapters(docs)
	if len(u.Chapters) < 3 {
		u.Contiguous = false
	}
	return u
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

func anyNumbered(docs []Doc) bool {
	for _, d := range docs {
		if d.Chapter > 0 {
			return true
		}
	}
	return false
}

func countNumbered(docs []Doc) int {
	n := 0
	for _, d := range docs {
		if d.Chapter > 0 {
			n++
		}
	}
	return n
}

// storySpan returns the index of the first and last NUMBERED section. Everything
// outside that span is apparatus.
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
	return first, last
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
	return hasExcerptCue(label)
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
	if hasExcerptCue(d.Label) {
		return true
	}
	return d.Words >= excerptWordFloor && !isBoilerplate(d.Label)
}

// isBoilerplate reports whether a label names apparatus that is long by nature.
func isBoilerplate(label string) bool {
	low := strings.ToLower(label)
	for _, cue := range boilerplateCues {
		if strings.Contains(low, cue) {
			return true
		}
	}
	return false
}

func hasExcerptCue(label string) bool {
	low := strings.ToLower(label)
	for _, cue := range excerptCues {
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
func quarantineEdges(docs []Doc, first, last int, u *Universe) {
	if first < 0 {
		for i := range docs {
			docs[i].Quarantine = quarantineNoStory
		}
		return
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
			u.Suspected = true
		default:
			docs[i].Quarantine = quarantineBack
		}
	}
	if u.Suspected {
		u.Notes = append(u.Notes, "a trailing section looks like an excerpt of a different book and was excluded; "+
			"confirm before contributing, since another book's text must never reach the fact pass")
	}
}

// assignOrdinals numbers the story sections 1..N in reading order.
func assignOrdinals(docs []Doc, first, last int) {
	if first < 0 {
		return
	}
	n := 0
	for i := first; i <= last; i++ {
		if docs[i].Quarantine != "" {
			continue
		}
		n++
		docs[i].Chapter, docs[i].Source = n, SourceOrdinal
	}
}

// foldContinuations attaches an unnumbered section BETWEEN two numbered ones to the
// chapter before it: a chapter split across several spine files is common, and its
// later files carry no toc entry of their own.
func foldContinuations(docs []Doc, first, last int) {
	if first < 0 {
		return
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

// collectChapters folds the sections into logical chapters, in chapter order.
func collectChapters(docs []Doc) []Chapter {
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
		if c.Title == "" && d.Source != SourceContinuation {
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
		out = append(out, *byNum[n])
	}
	return out
}

// chapterStem is the per-chapter filename stem, matching the audio path's chNNN.
func chapterStem(n int) string { return fmt.Sprintf("ch%03d", n) }

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
