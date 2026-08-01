package pipeline

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// nonNarrativeWordThreshold is the word count below which an EDGE audio file is
// treated as non-narrative (an opening announcement / closing credits) rather than a
// story chapter. A real story chapter runs to thousands of words; the incident's
// Audible intro transcribed to 3 words and its closing credits to 35-80, so a low
// floor cleanly separates them without touching a genuine (short) chapter. It gates
// only maximal LEADING/TRAILING runs, never an interior chapter, and only in concert
// with the short-duration corroboration below.
const nonNarrativeWordThreshold = 120

// edgeNonNarrativeMaxDurationSec is the SHORT-DURATION corroboration for a non-narrative
// edge file: an opening announcement / closing credits file is seconds long (the incident's
// intro was 2s, its credits 65s), while a genuinely short REAL chapter (a brief epilogue)
// still runs to minutes. Requiring an excluded edge file to be under this bound AS WELL AS
// under the word threshold stops a sub-120-word real chapter from being silently excluded and
// affirmatively mislabeled "closing credits". A missing/zero manifest duration does NOT block
// exclusion (some manifests omit per-chapter durations): an unknown duration is treated as
// satisfying the check, preserving the word-only behaviour where no duration is available.
const edgeNonNarrativeMaxDurationSec = 180.0

// edgeChapter is one manifest chapter's classification input, in manifest order.
// HasTranscript is false when no readable transcript exists for the chapter; such a
// chapter is treated as NARRATIVE (never excluded) so a missing transcript can never
// silently drop a real chapter. Probed is false for an unprobed interior chapter of a large
// book (classifyBookEdges word-counts only the edges): an unprobed chapter is a narrative
// sentinel and can never be non-narrative, so the classifier represents "not looked at"
// explicitly instead of with an in-band magic word count. DurationSec is the chapter's manifest
// audio length (0 when the manifest omits it) for the short-duration corroboration.
type edgeChapter struct {
	Chapter       int
	Words         int
	HasTranscript bool
	Probed        bool
	DurationSec   float64
	// Opening is the first few hundred characters of the chapter's transcript, carried so the
	// classifier can read the chapter number the narrator ANNOUNCES (see deriveNarratedNumbering).
	// Empty for an unprobed chapter or one with no transcript.
	Opening string
}

// edgeClassification is the result of classifying a files-style book's edge chapters:
// the LOGICAL story-chapter count (narrative chapters only), the excluded leading and trailing
// chapter numbers, and the stage-appropriate agent notes. EdgeNote is the GENERIC note used by
// the spoken-number stages (synthesis / audit / audit_verify / fix, which reason purely in
// spoken chapter numbers); ChunkNote is the fact-pass CHUNK note (headings/attributions stay on
// file numbers, spoken numbers only in fact text); AssembleNote is the fact-pass ASSEMBLE note,
// the ONE renumbering boundary carrying the concrete file->spoken mapping. When nothing is
// excluded LogicalCount equals the raw chapter count and every note is "" (the
// overwhelming-majority case: prompts render byte-identically to before).
type edgeClassification struct {
	LogicalCount     int
	ExcludedLeading  []int
	ExcludedTrailing []int
	// FrontMatter and EndMatter are retained NARRATIVE audio files that are not NUMBERED
	// chapters: an unnumbered Prologue before chapter 1, an Epilogue or bonus section after the
	// last numbered chapter. They hold real story content (so they are never excluded like
	// intro/credits are) but they do not consume a chapter position - front matter is the meta
	// schema's position 0. Both are empty unless deriveNarratedNumbering found the numbering.
	FrontMatter []int
	EndMatter   []int
	// ChapterOffset is the derived file->chapter shift at the START of the numbered range:
	// audio file N holds spoken chapter N-ChapterOffset. It is len(ExcludedLeading) when the
	// narration gave no usable evidence. It describes the WHOLE book only when Positions is
	// a constant shift (see ConstantOffset) - a book carrying unnumbered interior sections
	// has no single offset.
	ChapterOffset int
	// Positions maps a manifest chapter number to the spoken story chapter it belongs to,
	// for every retained file that has one. It is empty when the narration gave no usable
	// evidence, in which case position IS the manifest number less ChapterOffset.
	//
	// It exists because a constant offset cannot express a book with unnumbered material
	// in the MIDDLE. An interlude between chapters 9 and 10 belongs to chapter 10: rounding
	// UP is what keeps a spoiler gated, since a listener who has finished chapter 10 has
	// necessarily heard the interlude, while one who has only finished 9 has not.
	Positions    map[int]int
	EdgeNote     string
	ChunkNote    string
	AssembleNote string
}

// PositionOf returns the spoken story chapter a manifest chapter belongs to, and whether
// it has one at all (end matter does not - it sits past the last numbered chapter).
func (c edgeClassification) PositionOf(chapter int) (int, bool) {
	if len(c.Positions) == 0 {
		return chapter - c.ChapterOffset, true
	}
	p, ok := c.Positions[chapter]
	return p, ok
}

// ConstantOffset reports whether every positioned chapter sits at the same shift, so the
// mapping can be stated as one subtraction rather than as a table.
func (c edgeClassification) ConstantOffset() bool {
	for ch, pos := range c.Positions {
		if ch-pos != c.ChapterOffset {
			return false
		}
	}
	return true
}

// classifyEdgeChapters classifies the maximal LEADING and TRAILING runs of non-narrative
// chapters (probed, transcript present, word count below the threshold, AND short duration),
// never an interior chapter. It returns no exclusions (the raw count) in two guard cases:
//   - PROBE-WINDOW SATURATION: a leading or trailing run reaches edgeProbeDepth, meaning no
//     narrative chapter was found within the probe window (a large book of uniformly short
//     chapters, or an unusually deep edge run). A real intro/credits edge is 1-2 files, so a
//     run this long is unreliable - exclude nothing rather than blank most of the book.
//   - DEGENERATE all-short book: the leading and trailing runs cover the whole book, so no
//     narrative chapter would remain.
//
// It is pure so the threshold, duration and edge rules stay table-testable.
func classifyEdgeChapters(chs []edgeChapter) edgeClassification {
	n := len(chs)
	if n == 0 {
		return edgeClassification{}
	}
	lead := 0
	for lead < n && nonNarrative(chs[lead]) {
		lead++
	}
	trail := 0
	for trail < n && nonNarrative(chs[n-1-trail]) {
		trail++
	}
	// Probe-window saturation: a run reaching the probe depth means no narrative chapter was
	// found within reach, so the classification is unreliable (see the doc comment). This also
	// catches a large book of uniformly sub-threshold short chapters that the degenerate
	// all-short fallback below cannot see (its probe window never covers the whole book).
	if lead >= edgeProbeDepth || trail >= edgeProbeDepth {
		return edgeClassification{LogicalCount: n}
	}
	// Degenerate fallback: the leading and trailing runs cover the whole book, so no narrative
	// chapter remains. Exclude nothing rather than blank the work.
	if lead+trail >= n {
		return edgeClassification{LogicalCount: n}
	}
	var leading []int
	for i := 0; i < lead; i++ {
		leading = append(leading, chs[i].Chapter)
	}
	var trailing []int
	for i := n - trail; i < n; i++ { // ascending order
		trailing = append(trailing, chs[i].Chapter)
	}
	logical := n - lead - trail
	offset := lead
	var frontMatter, endMatter []int
	var positions map[int]int
	// Prefer the numbering the narration actually announces over counting positions. Only a
	// corroborated run overrides, and only when it leaves at least one numbered chapter.
	if num, ok := deriveNarratedNumbering(chs, lead, trail); ok && num.last >= 1 {
		offset = num.offset
		logical = num.last
		frontMatter, endMatter, positions = num.frontMatter, num.endMatter, num.positions
	}
	class := edgeClassification{
		LogicalCount:     logical,
		ExcludedLeading:  leading,
		ExcludedTrailing: trailing,
		FrontMatter:      frontMatter,
		EndMatter:        endMatter,
		ChapterOffset:    offset,
		Positions:        positions,
	}
	class.EdgeNote = composeEdgeNote(logical, leading, trailing, frontMatter, endMatter)
	class.ChunkNote = composeChunkNote(leading, trailing)
	class.AssembleNote = composeAssembleNote(class)
	return class
}

// narratedNumbering is the file->chapter mapping read out of the narration.
type narratedNumbering struct {
	positions   map[int]int // manifest chapter number -> spoken story chapter
	frontMatter []int
	endMatter   []int
	last        int // the highest spoken chapter number
	offset      int // the shift at the first numbered file
}

// minAnnouncementRun is how many CONSECUTIVE audio files must announce consecutive chapter
// numbers under one offset before that numbering is trusted. A single announcement is not
// evidence - a chapter can open "One more time, Joe" - but three files running N, N+1, N+2
// under a constant offset cannot be produced by prose coincidence. Below this the classifier
// keeps its previous positional assumption.
const minAnnouncementRun = 3

// deriveNarratedNumbering reads the chapter number the narration ANNOUNCES in each probed
// retained file and derives the real file->chapter mapping from it, rather than assuming every
// retained file is a numbered chapter.
//
// This exists because position among the retained files is NOT the chapter number whenever a
// book carries unnumbered narrative sections. The live case: an Audible intro (file 1,
// excluded as non-narrative), an unnumbered Prologue (file 2), chapters 1-59 (files 3-61), an
// Epilogue (62), a bloopers reel (63) and credits (64, excluded). Positional counting made
// that 62 logical chapters at offset 1, so every reveal and recap gate landed a chapter early
// and the audit correctly rejected them as disclosing later material, round after round. The
// narration says "One."/"Two."/"Three." in files 3/4/5, which fixes the offset at 2, and
// "Fifty-nine." in file 61, which fixes the numbered range at 59.
//
// A single offset cannot describe a book with unnumbered material in the MIDDLE (an
// Interlude between chapters 9 and 10, a chapter split into "10a" and "10b"), so the mapping
// is per file. An unnumbered interior file takes the number of the NEXT announced chapter:
// rounding up is the direction that cannot leak, because finishing that chapter implies
// having heard the section, whereas the preceding chapter does not.
//
// It is pure (openings are carried on edgeChapter) and conservative: with no agreeing run it
// reports ok=false and nothing changes, and an announcement is only believed when it is
// consistent with the corroborated run, so one chapter opening "One more time" cannot shift a
// book.
func deriveNarratedNumbering(chs []edgeChapter, lead, trail int) (narratedNumbering, bool) {
	n := len(chs)
	announced := make(map[int]int, n) // index -> announced chapter number
	for i := lead; i < n-trail; i++ {
		// Deliberately NOT gated on Probed: an opening is read for every chapter (see
		// classifyBookEdges), and the numbering is only as reliable as its coverage.
		if chs[i].Opening == "" {
			continue
		}
		if num, got := audio.SpokenChapterNumber(chs[i].Opening); got {
			announced[i] = num
		}
	}
	// The longest run of consecutive files whose announcements also run consecutively. Both
	// must hold: consecutive files announcing 7, 7, 7 (a stuck transcript) is not a numbering.
	// That run is the ANCHOR - the only announcements trusted without corroboration.
	bestLen, bestStart := 0, -1
	runLen := 0
	for i := lead; i < n-trail; i++ {
		num, got := announced[i]
		if !got {
			runLen = 0
			continue
		}
		if runLen > 0 && announced[i-1] == num-1 && chs[i].Chapter == chs[i-1].Chapter+1 {
			runLen++
		} else {
			runLen = 1
		}
		if runLen > bestLen {
			bestLen, bestStart = runLen, i-runLen+1
		}
	}
	if bestLen < minAnnouncementRun {
		return narratedNumbering{}, false
	}

	// Keep the anchor run, then extend outward, believing a further announcement only while
	// it stays monotone with what is already established. A stray parse outside the run is
	// dropped rather than allowed to renumber the book.
	kept := make(map[int]int, n)
	for i := bestStart; i < bestStart+bestLen; i++ {
		kept[i] = announced[i]
	}
	first, last := bestStart, bestStart+bestLen-1
	for i := last + 1; i < n-trail; i++ {
		if num, got := announced[i]; got && num >= kept[last] {
			kept[i], last = num, i
		}
	}
	for i := first - 1; i >= lead; i-- {
		if num, got := announced[i]; got && num <= kept[first] {
			kept[i], first = num, i
		}
	}

	// Every retained file now takes a position, and three cases must not be confused:
	//
	//   - Read, and announces nothing: genuinely unnumbered narrative, so it belongs to the
	//     next announced chapter (rounding up, which cannot leak).
	//   - Read, announced a number that was NOT believed: still a numbered chapter - it did
	//     open with a number - so it keeps the shift in force.
	//   - Not read at all (no transcript): silence is not evidence, so it keeps the shift too.
	//
	// Folding the last two in with the first collapsed a 59-chapter book onto chapter 55.
	num := narratedNumbering{
		positions: make(map[int]int, n),
		last:      kept[last],
		offset:    chs[first].Chapter - kept[first],
	}
	nextAnnounced := make(map[int]int, n)
	for i, next := n-trail-1, 0; i >= lead; i-- {
		if k, got := kept[i]; got {
			next = k
		}
		nextAnnounced[i] = next
	}
	shift := chs[first].Chapter - kept[first]
	for i := lead; i < n-trail; i++ {
		if announcedHere, got := kept[i]; got {
			shift = chs[i].Chapter - announcedHere
			num.positions[chs[i].Chapter] = announcedHere
			continue
		}
		_, parsed := announced[i]
		switch {
		// A file that ANNOUNCED a number which was not believed (it contradicted the
		// corroborated run) is still a numbered chapter - it opened with a number - so it
		// keeps the shift in force rather than being folded into its neighbour. Same for a
		// file with no transcript to read at all: silence is not evidence of anything.
		case parsed || chs[i].Opening == "":
			num.positions[chs[i].Chapter] = chs[i].Chapter - shift
		case i > last:
			num.endMatter = append(num.endMatter, chs[i].Chapter)
		case i < first:
			num.frontMatter = append(num.frontMatter, chs[i].Chapter)
			num.positions[chs[i].Chapter] = 0
		// Read, and announced nothing: an unnumbered narrative section, which belongs to the
		// chapter it precedes.
		default:
			num.positions[chs[i].Chapter] = nextAnnounced[i]
		}
	}
	return num, true
}

// nonNarrative reports whether an edge chapter is a non-narrative intro/credits file: it must
// be PROBED (an unprobed interior sentinel never qualifies, so a book whose interior was not
// word-counted can never be blanked), have a transcript, fall below the word threshold, AND be
// short in duration (a real short chapter runs minutes; an unknown duration counts as short).
func nonNarrative(c edgeChapter) bool {
	return c.Probed && c.HasTranscript && c.Words < nonNarrativeWordThreshold && edgeDurationShort(c.DurationSec)
}

// edgeDurationShort reports whether a duration satisfies the short-duration corroboration. An
// unknown/zero duration (some manifests omit per-chapter durations) counts as short so the
// classifier keeps its word-only behaviour where no duration is available.
func edgeDurationShort(durationSec float64) bool {
	return durationSec <= 0 || durationSec < edgeNonNarrativeMaxDurationSec
}

// edgeFilesPhrase renders the shared opening sentence naming the non-narrative audio files and
// what they are: "Audio files 1 and 78 are non-narrative (opening announcement / closing
// credits), not story chapters." All three stage notes open with it.
func edgeFilesPhrase(leading, trailing []int) string {
	files := make([]int, 0, len(leading)+len(trailing))
	files = append(files, leading...)
	files = append(files, trailing...)
	// A book can now carry unnumbered narrative sections with NOTHING excluded (a prologue but
	// no Audible intro), so the notes reach here with no files to name.
	if len(files) == 0 {
		return ""
	}
	var kinds []string
	if len(leading) > 0 {
		kinds = append(kinds, "opening announcement")
	}
	if len(trailing) > 0 {
		kinds = append(kinds, "closing credits")
	}
	noun, verb := "Audio file", "is"
	if len(files) > 1 {
		noun, verb = "Audio files", "are"
	}
	return fmt.Sprintf("%s %s %s non-narrative (%s), not story chapters.",
		noun, joinIntsAnd(files), verb, strings.Join(kinds, " / "))
}

// logicalRangeText renders the logical story-chapter range: "1" for a single chapter, else "1-N".
func logicalRangeText(logical int) string {
	if logical > 1 {
		return fmt.Sprintf("1-%d", logical)
	}
	return "1"
}

// composeEdgeNote renders the GENERIC note the spoken-number stages inject (synthesis / audit /
// audit_verify / fix, which reason purely in spoken chapter numbers). It is "" when nothing was
// excluded. Otherwise it names the non-narrative files and the logical story-chapter range as
// spoken in the narration.
func composeEdgeNote(logical int, leading, trailing, frontMatter, endMatter []int) string {
	if len(leading) == 0 && len(trailing) == 0 && len(frontMatter) == 0 && len(endMatter) == 0 {
		return ""
	}
	return joinSentences(
		edgeFilesPhrase(leading, trailing),
		fmt.Sprintf("The work's logical story chapters are %s as spoken in the narration; facts and positions use those spoken chapter numbers.", logicalRangeText(logical)),
		unnumberedPhrase(frontMatter, endMatter, logical),
	)
}

// joinSentences joins the non-empty note sentences with a single space, so a note stays
// well-formed whichever of its parts apply to a given book.
func joinSentences(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

// composeChunkNote renders the fact-pass CHUNK note. A chunk agent's `## Chapter N` headings and
// [chN @ ...] attributions MUST stay on the audio-FILE numbers that match the staged transcript
// filenames and the chunk range - validateFactPassChunk enforces exactly one heading per file
// number in range and none outside, so a spoken-number heading would be unsatisfiable. This note
// tells the agent to never renumber a heading, that the narration's spoken chapter announcements
// may be offset from the file numbers (leading non-narrative files) and that offset is expected,
// that story events in the fact TEXT use the numbers the narration itself speaks, and that an
// excluded non-narrative file keeps its required heading with a single non-narrative line and no
// story facts. It is "" when nothing is excluded.
func composeChunkNote(leading, trailing []int) string {
	if len(leading) == 0 && len(trailing) == 0 {
		return ""
	}
	return edgeFilesPhrase(leading, trailing) +
		" Keep your `## Chapter N` headings and every `[chN @ ...]` attribution on the audio-FILE" +
		" numbers that match the staged transcript filenames and this chunk's range - never" +
		" renumber a heading. The narration's spoken chapter announcements may be offset from the" +
		" file numbers because of those non-narrative files, and that offset is expected; the" +
		" assembly stage renumbers to spoken chapters later. Within the fact TEXT, refer to story" +
		" events by the chapter numbers the narration itself speaks. For an excluded non-narrative" +
		" file that falls in your range, keep its required `## Chapter N` heading but record only a" +
		" single line noting it is a non-narrative file (opening announcement or closing credits)" +
		" with no story facts."
}

// composeAssembleNote renders the fact-pass ASSEMBLE note - the ONE renumbering boundary. The
// chunk fact files key their `## Chapter N` headings and [chN] attributions to audio-FILE
// numbers; the final knowledge sheet must be keyed to SPOKEN story chapter numbers. When there
// is a LEADING exclusion, audio file N holds spoken chapter N-offset (offset = the number of
// leading non-narrative files, computed here in code), so the note states that concrete mapping
// rather than a formula for the agent to derive. When only trailing files are excluded the file
// numbers already equal the spoken numbers for the narrative range, so no renumbering is needed.
// It is "" when nothing is excluded.
// unnumberedPhrase renders the trailing sentence naming retained-but-unnumbered narrative
// files: an unnumbered Prologue is the meta schema's front matter (position 0), and an
// Epilogue or bonus section past the last numbered chapter has no chapter position at all -
// its material belongs in the whole-book summaries, never in a chapter-gated entry (gating it
// at the final chapter would leak the ending to a listener who has only finished that
// chapter). It is "" when the numbering derivation found neither.
func unnumberedPhrase(frontMatter, endMatter []int, logical int) string {
	var parts []string
	if len(frontMatter) > 0 {
		noun, verb := "file", "holds"
		if len(frontMatter) > 1 {
			noun, verb = "files", "hold"
		}
		parts = append(parts, fmt.Sprintf(
			"Audio %s %s %s narrative front matter (a prologue or similar) that the narration does NOT number:"+
				" it is chapter position 0, not chapter 1, and it never consumes a chapter number.",
			noun, joinIntsAnd(frontMatter), verb))
	}
	if len(endMatter) > 0 {
		noun, verb := "file", "is"
		if len(endMatter) > 1 {
			noun, verb = "files", "are"
		}
		parts = append(parts, fmt.Sprintf(
			"Audio %s %s %s narrative material AFTER the last numbered chapter (an epilogue or bonus section)"+
				" and %s outside the numbered range: never emit a position above %d for it, and place anything"+
				" from it in the whole-book summaries rather than a chapter-gated entry.",
			noun, joinIntsAnd(endMatter), verb, verb, logical))
	}
	return joinSentences(parts...)
}

func composeAssembleNote(c edgeClassification) string {
	logical, offset := c.LogicalCount, c.ChapterOffset
	if len(c.ExcludedLeading) == 0 && len(c.ExcludedTrailing) == 0 &&
		len(c.FrontMatter) == 0 && len(c.EndMatter) == 0 && c.ConstantOffset() {
		return ""
	}
	prefix := joinSentences(
		edgeFilesPhrase(c.ExcludedLeading, c.ExcludedTrailing),
		unnumberedPhrase(c.FrontMatter, c.EndMatter, logical))
	// A book whose unnumbered material sits only at the ENDS shifts by one constant, so the
	// mapping states as a single subtraction. One with unnumbered material in the MIDDLE (an
	// interlude, a split "10a"/"10b") does not, and must be given the mapping explicitly -
	// inventing a formula for it is exactly the error that put every gate a chapter out.
	if !c.ConstantOffset() {
		return joinSentences(prefix, fmt.Sprintf(
			"This is the one renumbering boundary: the fact files' [chN] attributions are FILE"+
				" numbers, and this book's audio files do NOT shift onto the spoken story chapters"+
				" by a single constant, because it carries unnumbered sections between numbered"+
				" chapters. The mapping is: %s. An unnumbered section belongs to the chapter it"+
				" precedes, and two files of one chapter share that chapter's number. Write the"+
				" final knowledge sheet keyed by SPOKEN story chapter numbers %s.",
			describeFileRuns(c), logicalRangeText(logical)))
	}
	if offset == 0 {
		return joinSentences(prefix, fmt.Sprintf(
			"The fact files' [chN] attributions are already the spoken story-chapter numbers;"+
				" build knowledge-final.md from spoken story chapters %s and treat the trailing"+
				" non-narrative file(s) as outside the story.", logicalRangeText(logical)))
	}
	return joinSentences(prefix, fmt.Sprintf(
		"This is the one renumbering boundary: audio file N contains spoken story chapter N-%d"+
			" (files %d-%d are the %d story chapters), and the fact files' [chN] attributions are"+
			" FILE numbers. Write the final knowledge sheet keyed by SPOKEN story chapter numbers"+
			" %s, subtracting %d from each fact file's chapter number.",
		offset, offset+1, offset+logical, logical, logicalRangeText(logical), offset))
}

// describeFileRuns renders a non-constant mapping compactly, by grouping consecutive files
// that share one shift: "audio files 1-8 hold chapters 1-8; files 9-11 hold chapters 8-10".
// Listing every file individually would run to hundreds of pairs on a real book.
func describeFileRuns(c edgeClassification) string {
	files := make([]int, 0, len(c.Positions))
	for ch := range c.Positions {
		files = append(files, ch)
	}
	sort.Ints(files)

	var parts []string
	flush := func(from, to int) {
		fromPos, toPos := c.Positions[from], c.Positions[to]
		noun := fmt.Sprintf("audio file %d", from)
		if from != to {
			noun = fmt.Sprintf("audio files %d-%d", from, to)
		}
		what := fmt.Sprintf("chapter %d", fromPos)
		if fromPos != toPos {
			what = fmt.Sprintf("chapters %d-%d", fromPos, toPos)
		}
		parts = append(parts, noun+" hold "+what)
	}
	runFrom := files[0]
	for i := 1; i <= len(files); i++ {
		contiguousRun := i < len(files) &&
			files[i] == files[i-1]+1 &&
			c.Positions[files[i]]-files[i] == c.Positions[files[i-1]]-files[i-1]
		if !contiguousRun {
			flush(runFrom, files[i-1])
			if i < len(files) {
				runFrom = files[i]
			}
		}
	}
	return strings.Join(parts, "; ")
}

// joinIntsAnd renders a small int list in prose: "1", "1 and 78", "1, 2 and 78".
func joinIntsAnd(ns []int) string {
	strs := make([]string, len(ns))
	for i, n := range ns {
		strs[i] = strconv.Itoa(n)
	}
	switch len(strs) {
	case 0:
		return ""
	case 1:
		return strs[0]
	default:
		// "1 and 78" for two, "1, 2 and 78" for more - the default covers both.
		return strings.Join(strs[:len(strs)-1], ", ") + " and " + strs[len(strs)-1]
	}
}

// edgeProbeDepth is how many chapters at each end classifyBookEdges word-counts when a
// book has more than 2*edgeProbeDepth chapters. Only the maximal LEADING and TRAILING
// non-narrative runs can ever be excluded, and a real edge run is 1-2 short intro/credits
// files (an 8-deep run of sub-threshold edge files does not occur in practice), so an
// unprobed interior chapter is safely treated as narrative without reading its transcript.
// A leading or trailing run that reaches this depth saturates the probe window and is treated
// as unreliable (classifyEdgeChapters returns no exclusions).
const edgeProbeDepth = 8

// classifyBookEdges is the IO wrapper: it reads the book's manifest, word-counts the
// edge chapters (transcript presence + count, repaired preferred over text via the shared
// transcript resolver, same source as chapterWordCount), carries each edge chapter's manifest
// duration for the short-duration corroboration, and classifies the edges. A manifest or read
// error is returned loudly.
//
// It runs for EVERY style because the pathology is content-driven, not style-driven.
// A StyleFiles book has one synthesized chapter per audio file (a 2-second Audible
// intro and a credits file each become a phantom "chapter"); a StyleMarkers book can
// hit the same shape when its embedded markers are bare contiguous numbers ("001".."078")
// that markers_normalizing kept, intro and credits included. In both cases the edge
// files are short (well under the threshold) while real chapters run to thousands of
// words, so the classifier excludes them and no-ops on a book whose markers already
// exclude intro/credits (those edge chapters are narrative). The degenerate fallback and the
// probe-window saturation guard protect any book that would otherwise be left with no (or a
// single) narrative chapter.
//
// For a large book (> 2*edgeProbeDepth chapters) only the first and last edgeProbeDepth
// chapters are word-counted; each unprobed interior chapter enters the classifier as an
// unprobed narrative sentinel (Probed:false, so it can never belong to a maximal edge run). A
// small book is probed in full so the degenerate all-small fallback still sees every chapter.
func classifyBookEdges(workDir string) (edgeClassification, error) {
	m, err := audio.ReadManifest(workDir)
	if err != nil {
		return edgeClassification{}, fmt.Errorf("edge classify: read manifest (inspect must run first): %w", err)
	}
	if m.Style == audio.StyleEbook {
		return classifyEbookEdges(m)
	}
	n := len(m.Chapters)
	chs := make([]edgeChapter, 0, n)
	for i, ch := range m.Chapters {
		// The chapter's OPENING is read for EVERY chapter, not just the edges. Word counting
		// reads a whole transcript, so it stays bounded to the edges where it is needed; an
		// opening is 256 bytes, and the numbering derived from it is only as good as its
		// coverage. Reading it at the edges alone left the entire interior of a long book
		// unnumbered evidence-wise, so any unnumbered section in the middle had to be guessed
		// around - and a book carrying interludes is exactly the shape that needs it.
		opening, err := chapterOpening(workDir, ch.Chapter)
		if err != nil {
			return edgeClassification{}, fmt.Errorf("edge classify: read chapter %d opening: %w", ch.Chapter, err)
		}
		if n > 2*edgeProbeDepth && i >= edgeProbeDepth && i < n-edgeProbeDepth {
			// Unprobed interior narrative sentinel (Probed:false, so nonNarrative is false). It
			// bounds any edge run before it can reach the interior without reading a transcript.
			chs = append(chs, edgeChapter{Chapter: ch.Chapter, Opening: opening})
			continue
		}
		words, present, err := chapterWordCount(workDir, ch.Chapter)
		if err != nil {
			return edgeClassification{}, fmt.Errorf("edge classify: count chapter %d words: %w", ch.Chapter, err)
		}
		chs = append(chs, edgeChapter{Chapter: ch.Chapter, Words: words, HasTranscript: present, Probed: true, DurationSec: ch.Duration, Opening: opening})
	}
	return classifyEdgeChapters(chs), nil
}

// chapterOpeningBytes is how much of a chapter transcript is read to find its announcement.
// The announcement is the first few words; this is generous enough to survive leading
// whitespace and a long first token while staying a cheap read.
const chapterOpeningBytes = 256

// chapterOpening returns the first chapterOpeningBytes of a chapter's transcript (the same
// repaired-preferred resolver chapterWordCount uses), for reading the narrator's chapter
// announcement. A chapter with no transcript returns "" and no error.
func chapterOpening(workDir string, chapter int) (string, error) {
	p, ok := transcript.ChapterTextPath(workDir, chapter)
	if !ok {
		return "", nil
	}
	f, err := os.Open(p) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	buf := make([]byte, chapterOpeningBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	return string(buf[:n]), nil
}

// noteEdgeExclusions surfaces the edge-chapter classification's exclusions on the calling
// stage's event log so an operator can see WHY the logical chapter count is below the raw file
// count (a non-narrative intro/credits file was mechanically dropped) - the observability the
// silent exclusion previously lacked. It is a no-op when nothing was excluded (the overwhelming
// majority) or when the stage has no Note sink. Emitting it once per stage entry (a little
// duplication across the sidecar stages) is deliberate: each stage's log stays self-explaining.
func noteEdgeExclusions(r scheduler.StageReport, class edgeClassification) {
	if r.Note == nil {
		return
	}
	if len(class.ExcludedLeading) == 0 && len(class.ExcludedTrailing) == 0 &&
		len(class.FrontMatter) == 0 && len(class.EndMatter) == 0 {
		return
	}
	msg := fmt.Sprintf("edge classification: %d logical story chapter(s)", class.LogicalCount)
	if files := append(append([]int{}, class.ExcludedLeading...), class.ExcludedTrailing...); len(files) > 0 {
		msg += fmt.Sprintf("; excluded non-narrative audio file(s) %s (opening announcement / closing credits)", intsCSV(files))
	}
	// The derived mapping is the load-bearing number for every downstream chapter position, so
	// name it and the unnumbered sections explicitly rather than leaving an operator to infer
	// them from the count.
	if len(class.FrontMatter) > 0 {
		msg += fmt.Sprintf("; unnumbered front matter audio file(s) %s (chapter position 0)", intsCSV(class.FrontMatter))
	}
	if len(class.EndMatter) > 0 {
		msg += fmt.Sprintf("; unnumbered end matter audio file(s) %s (past the last numbered chapter)", intsCSV(class.EndMatter))
	}
	msg += fmt.Sprintf("; audio file N holds spoken chapter N-%d", class.ChapterOffset)
	r.Note(msg)
}

// classifyEbookEdges is the ebook counterpart: the manifest IS the logical universe,
// so the count is all there is to derive.
//
// It exists because the audio classifier would be actively WRONG here, and wrong in
// a way nothing downstream could detect. That classifier excludes an edge chapter
// that is both short in words and short in duration - and an ebook manifest carries
// no durations, so the duration half of the test passes for every chapter. The word
// half alone would then exclude a genuinely short opening chapter, and
// composeAssembleNote would tell the assembler that file N holds spoken chapter N-1.
// Every fact would shift one chapter earlier, so every character reveal and every
// recap would be gated one chapter too soon: mechanically valid, structurally
// invisible, and a spoiler leak across the whole book.
//
// No such classification is needed anyway. extracting already quarantined the
// front matter, the back matter and any suspected cross-book excerpt from the
// table of contents, then numbered the survivors 1..N - so nothing INSIDE the
// manifest is non-story, and there is no file-to-chapter offset to explain.
//
// The one thing it does check is that there is a universe at all. Every caller
// treats LogicalCount as authoritative - it caps the positions validateSidecars
// accepts and it is the chapter count the assemble prompt states - so a manifest
// with no chapters would quietly publish sidecars bounded at chapter 0. Upstream
// invariants make that unreachable today (BuildUniverse refuses a run below
// minChapters, extracting materializes only a contiguous one, validateChapterMap
// rejects fewer than two chapters), but none of them are enforced at this read.
func classifyEbookEdges(m audio.Manifest) (edgeClassification, error) {
	if len(m.Chapters) == 0 {
		return edgeClassification{}, fmt.Errorf("manifest %s lists no chapters; extracting must record the chapter universe before the authoring stages read it", audio.ManifestName)
	}
	return edgeClassification{LogicalCount: len(m.Chapters)}, nil
}
