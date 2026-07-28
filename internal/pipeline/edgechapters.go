package pipeline

import (
	"errors"
	"fmt"
	"io"
	"os"
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
	// ChapterOffset is the derived file->chapter shift: audio file N holds spoken chapter
	// N-ChapterOffset. It is len(ExcludedLeading) when the narration gave no usable evidence.
	ChapterOffset int
	EdgeNote      string
	ChunkNote     string
	AssembleNote  string
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
	// Prefer the numbering the narration actually announces over counting positions. Only a
	// corroborated run overrides, and only when it leaves at least one numbered chapter.
	if got, lastNumbered, ok := deriveNarratedNumbering(chs, lead, trail); ok && lastNumbered-got >= 1 {
		offset = got
		logical = lastNumbered - got
		for i := lead; i < n-trail; i++ {
			switch {
			case chs[i].Chapter <= got:
				frontMatter = append(frontMatter, chs[i].Chapter)
			case chs[i].Chapter > lastNumbered:
				endMatter = append(endMatter, chs[i].Chapter)
			}
		}
	}
	return edgeClassification{
		LogicalCount:     logical,
		ExcludedLeading:  leading,
		ExcludedTrailing: trailing,
		FrontMatter:      frontMatter,
		EndMatter:        endMatter,
		ChapterOffset:    offset,
		EdgeNote:         composeEdgeNote(logical, leading, trailing, frontMatter, endMatter),
		ChunkNote:        composeChunkNote(leading, trailing),
		AssembleNote:     composeAssembleNote(logical, leading, trailing, frontMatter, endMatter, offset),
	}
}

// minAnnouncementRun is how many CONSECUTIVE audio files must announce consecutive chapter
// numbers under one offset before that numbering is trusted. A single announcement is not
// evidence - a chapter can open "One more time, Joe" - but three files running N, N+1, N+2
// under a constant offset cannot be produced by prose coincidence. Below this the classifier
// keeps its previous positional assumption.
const minAnnouncementRun = 3

// deriveNarratedNumbering reads the chapter number the narration ANNOUNCES in each probed
// retained file and derives the real file->chapter mapping from it, rather than assuming every
// retained file is a numbered chapter. It returns the offset (audio file N holds chapter
// N-offset) and the last file that is still a numbered chapter.
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
// It is pure (openings are carried on edgeChapter) and conservative in both directions: with
// no agreeing run it reports ok=false and nothing changes, and it only shrinks the numbered
// range when the TRAILING probe window positively identifies the last numbered file - a book
// whose trailing openings simply do not parse keeps every retained file.
func deriveNarratedNumbering(chs []edgeChapter, lead, trail int) (offset, lastNumbered int, ok bool) {
	n := len(chs)
	announced := make(map[int]int, n) // index -> announced chapter number
	for i := lead; i < n-trail; i++ {
		if !chs[i].Probed || chs[i].Opening == "" {
			continue
		}
		if num, got := audio.SpokenChapterNumber(chs[i].Opening); got {
			announced[i] = num
		}
	}
	// The longest run of consecutive files whose announcements also run consecutively. Both
	// must hold: consecutive files announcing 7, 7, 7 (a stuck transcript) is not a numbering.
	bestLen, bestOffset := 0, 0
	runLen, runOffset := 0, 0
	for i := lead; i < n-trail; i++ {
		num, got := announced[i]
		if !got {
			runLen = 0
			continue
		}
		thisOffset := chs[i].Chapter - num
		if runLen > 0 && thisOffset == runOffset && announced[i-1] == num-1 {
			runLen++
		} else {
			runLen, runOffset = 1, thisOffset
		}
		if runLen > bestLen {
			bestLen, bestOffset = runLen, runOffset
		}
	}
	if bestLen < minAnnouncementRun {
		return 0, 0, false
	}
	// The last numbered file, sought only within the TRAILING probe window: outside it the
	// openings were never read, so a leading-run match must not be mistaken for the end of the
	// book. No match in that window means "cannot tell" - keep every retained file.
	lastNumbered = chs[n-trail-1].Chapter
	for i := n - trail - 1; i >= lead && i >= n-edgeProbeDepth; i-- {
		if num, got := announced[i]; got && chs[i].Chapter-num == bestOffset {
			lastNumbered = chs[i].Chapter
			break
		}
	}
	return bestOffset, lastNumbered, true
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

func composeAssembleNote(logical int, leading, trailing, frontMatter, endMatter []int, offset int) string {
	if len(leading) == 0 && len(trailing) == 0 && len(frontMatter) == 0 && len(endMatter) == 0 {
		return ""
	}
	prefix := joinSentences(edgeFilesPhrase(leading, trailing), unnumberedPhrase(frontMatter, endMatter, logical))
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
	n := len(m.Chapters)
	chs := make([]edgeChapter, 0, n)
	for i, ch := range m.Chapters {
		if n > 2*edgeProbeDepth && i >= edgeProbeDepth && i < n-edgeProbeDepth {
			// Unprobed interior narrative sentinel (Probed:false, so nonNarrative is false). It
			// bounds any edge run before it can reach the interior without reading a transcript.
			chs = append(chs, edgeChapter{Chapter: ch.Chapter})
			continue
		}
		words, present, err := chapterWordCount(workDir, ch.Chapter)
		if err != nil {
			return edgeClassification{}, fmt.Errorf("edge classify: count chapter %d words: %w", ch.Chapter, err)
		}
		opening, err := chapterOpening(workDir, ch.Chapter)
		if err != nil {
			return edgeClassification{}, fmt.Errorf("edge classify: read chapter %d opening: %w", ch.Chapter, err)
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
