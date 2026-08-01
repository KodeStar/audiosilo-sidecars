package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/ebook"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// EbookChaptersNotConfidentPrefix labels the park a declined mapping produces.
const EbookChaptersNotConfidentPrefix = "chapter mapping was not confident"

// chaptersFileName is what the mapping agent writes its map to.
const chaptersFileName = "chapters.json"

// chapterMapPromptData feeds epubchapters.md.
type chapterMapPromptData struct {
	Title         string
	Authors       string
	Series        string
	SeriesPos     string
	SectionCount  int
	LabeledCount  int
	NumberedCount int
	HeadWords     int
}

// agentChapterMap is the mapping the agent returns.
type agentChapterMap struct {
	Chapters []struct {
		Chapter int      `json:"chapter"`
		Title   string   `json:"title"`
		Files   []string `json:"files"`
	} `json:"chapters"`
	Quarantine []struct {
		File   string `json:"file"`
		Reason string `json:"reason"`
	} `json:"quarantine"`
}

// chapterMapping derives an epub's logical chapter numbers when the toc labels did
// not yield a contiguous run on their own.
//
// It is markers_normalizing's counterpart, and follows it beat for beat because the
// two solve the same problem: a numbering scheme the deterministic parser does not
// know. That includes the free re-derivation below, the not-confident park, and the
// refusal to harvest anything but a fully-checked map.
func (e *Executor) chapterMapping(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	draft, err := ebook.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_mapping: read extract manifest (extracting must run first): %w", err)
	}
	splitDir := filepath.Join(book.WorkDir, ebook.ExtractDir)

	// Upgrade recovery, free. A newer label vocabulary may resolve outright what an
	// older one could not, so re-derive from the recorded sections before paying for
	// an agent round.
	//
	// Gated on the draft being non-contiguous, which is what makes it safe: a map the
	// agent already produced is contiguous by construction (the validator accepts
	// nothing else), so this can never overwrite the agent's work with a re-derivation.
	if !draft.Contiguous {
		if rebuilt := ebook.ReparseUniverse(draft); rebuilt.Contiguous {
			if err := e.materializeEbookChapters(ctx, book, rebuilt, splitDir, ""); err != nil {
				return scheduler.StageResult{}, err
			}
			// Record the recovered universe too, as the agent branch does. The extract
			// manifest is what a human reads to see why a book parked and what the
			// mapping agent is handed on a later round; leaving the superseded draft
			// there would have it contradict the chapters actually published.
			if err := ebook.WriteManifest(book.WorkDir, rebuilt); err != nil {
				return scheduler.StageResult{}, err
			}
			if r.Note != nil {
				r.Note(fmt.Sprintf("deterministic label reparse recovered %s - no agent round needed",
					countNoun(len(rebuilt.Chapters), "chapter")))
			}
			if r.Progress != nil {
				r.Progress(1, 1)
			}
			// NO RateSample: this is a sub-millisecond re-derivation, while the stage's
			// real cost is the agent round it skipped. Feeding it to the EWMA would drag
			// the learned chapter_mapping rate toward zero with every recovered book and
			// then under-predict every book that DOES need the agent. Same rule as the
			// markers reparse and the repair stage's free known-failed skip.
			result := scheduler.StageResult{Metrics: metrics(map[string]any{
				"chapters": len(rebuilt.Chapters), "deterministic_reparse": true,
			})}
			if err := scheduler.WriteSentinel(book.WorkDir, string(state.ChapterMapping), result); err != nil {
				return scheduler.StageResult{}, err
			}
			return result, nil
		}
	}

	if r.Note != nil {
		r.Note(fmt.Sprintf("mapping chapters over %s (%d labelled)",
			countNoun(len(draft.Docs), "section"), draft.Labeled))
	}
	st, err := agent.New(book.WorkDir, string(state.ChapterMapping), e.stageAttempt(ctx, book, state.ChapterMapping))
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if err := st.CopyFile(filepath.Join(book.WorkDir, ebook.ManifestName), ebook.ManifestName); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_mapping: stage extract manifest: %w", err)
	}

	// Every file the agent may reference. A map that names anything else is rejected:
	// an invented filename is an invented chapter.
	inputFiles := make(map[string]bool, len(draft.Docs))
	numbered := 0
	for _, d := range draft.Docs {
		inputFiles[d.File] = true
		if d.Chapter > 0 {
			numbered++
		}
	}

	validate := func(_ agent.Result, s *agent.Staging) error {
		// A not-confident verdict is a VALID terminal output: the agent followed the
		// prompt's decline instruction, so it legitimately wrote no map. Accept it
		// without a retry and let the post-run path park with its reason.
		v, verr := readMarkerVerdict(s.OutDir())
		if verr != nil {
			return fmt.Errorf("out/verdict.json: %v", verr)
		}
		if !v.Confident {
			return nil
		}
		return validateChapterMap(s.OutDir(), inputFiles)
	}

	data := chapterMapPromptData{
		Title:         book.Title,
		Authors:       authors(book),
		Series:        book.Series,
		SeriesPos:     book.SeriesPos,
		SectionCount:  len(draft.Docs),
		LabeledCount:  draft.Labeled,
		NumberedCount: numbered,
		HeadWords:     ebook.HeadWords,
	}
	usage, err := e.runAgent(ctx, book, state.ChapterMapping, r, st, "epubchapters.md", data, false, validate)
	if err != nil {
		return scheduler.StageResult{}, err
	}

	verdict, err := readMarkerVerdict(st.OutDir())
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_mapping: read verdict: %w", err)
	}
	if !verdict.Confident {
		// A human decision point, not a failure. Do NOT harvest: a guessed numbering
		// publishes wrong spoiler positions that look right.
		reason := strings.TrimSpace(verdict.Reason)
		if reason == "" {
			reason = "the agent could not produce a confident chapter mapping"
		}
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkEbookChaptersNotConfident,
			EbookChaptersNotConfidentPrefix+": "+reason)
	}

	mapped, err := readChapterMap(st.OutDir())
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_mapping: read map: %w", err)
	}
	final := applyChapterMap(draft, mapped)
	if err := ebook.WriteManifest(book.WorkDir, final); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_mapping: write manifest: %w", err)
	}
	if err := e.materializeEbookChapters(ctx, book, final, splitDir, ""); err != nil {
		return scheduler.StageResult{}, err
	}
	if r.Note != nil {
		r.Note(fmt.Sprintf("mapped %s (%d sections excluded)",
			countNoun(len(final.Chapters), "chapter"), quarantinedCount(final)))
	}
	if r.Progress != nil {
		r.Progress(1, 1)
	}

	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{
			"chapters": len(final.Chapters),
			"words":    final.Words,
			"usage":    usage.metricsMap(),
		}),
		RateSample: usage.rateSample(),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.ChapterMapping), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// applyChapterMap folds the agent's map back onto the recorded sections, so the
// persisted manifest keeps every section's provenance (label, words, spine, head)
// alongside its new verdict.
func applyChapterMap(draft ebook.Universe, m agentChapterMap) ebook.Universe {
	byFile := map[string]int{}
	titles := map[int]string{}
	for _, c := range m.Chapters {
		titles[c.Chapter] = strings.TrimSpace(c.Title)
		for _, f := range c.Files {
			byFile[f] = c.Chapter
		}
	}
	reasons := map[string]string{}
	for _, q := range m.Quarantine {
		reasons[q.File] = strings.TrimSpace(q.Reason)
	}

	out := draft
	out.Chapters = nil
	out.Docs = make([]ebook.Doc, len(draft.Docs))
	copy(out.Docs, draft.Docs)
	for i := range out.Docs {
		f := out.Docs[i].File
		if n, ok := byFile[f]; ok {
			out.Docs[i].Chapter, out.Docs[i].Source, out.Docs[i].Quarantine = n, ebook.SourceAgent, ""
			continue
		}
		reason := reasons[f]
		if reason == "" {
			reason = "excluded by chapter mapping"
		}
		out.Docs[i].Chapter, out.Docs[i].Source, out.Docs[i].Quarantine = 0, "", reason
	}
	out.Chapters = ebook.CollectChapters(out.Docs, titles)
	out.Contiguous = true // the validator accepted nothing else
	return out
}

// validateChapterMap is the mechanical gate on a confident mapping.
//
// Every rule here exists because breaking it produces a WRONG chapter number
// rather than an error, and a wrong chapter number is a wrong spoiler position in
// the published sidecars - which nothing downstream re-derives or re-checks.
func validateChapterMap(outDir string, inputFiles map[string]bool) error {
	m, err := readChapterMap(outDir)
	if err != nil {
		return err
	}

	if len(m.Chapters) < 2 {
		return fmt.Errorf("only %d chapters were mapped; a one-chapter map is a decline in disguise - "+
			"set confident:false with your reason instead", len(m.Chapters))
	}

	seen := map[string]string{} // file -> where it was claimed
	nums := make([]int, 0, len(m.Chapters))
	for _, c := range m.Chapters {
		if c.Chapter <= 0 {
			return fmt.Errorf("chapter number %d is not positive", c.Chapter)
		}
		if len(c.Files) == 0 {
			return fmt.Errorf("chapter %d lists no files", c.Chapter)
		}
		nums = append(nums, c.Chapter)
		for _, f := range c.Files {
			if !inputFiles[f] {
				return fmt.Errorf("chapter %d names file %q, which is not one of the split sections - "+
					"use only the `file` values from extract_manifest.json", c.Chapter, f)
			}
			if where, dup := seen[f]; dup {
				return fmt.Errorf("file %q appears in both %s and chapter %d; every section belongs to exactly one", f, where, c.Chapter)
			}
			seen[f] = fmt.Sprintf("chapter %d", c.Chapter)
		}
	}
	for _, q := range m.Quarantine {
		if !inputFiles[q.File] {
			return fmt.Errorf("quarantine names file %q, which is not one of the split sections", q.File)
		}
		if where, dup := seen[q.File]; dup {
			return fmt.Errorf("file %q is both quarantined and in %s; every section belongs to exactly one", q.File, where)
		}
		seen[q.File] = "quarantine"
	}
	if len(seen) != len(inputFiles) {
		missing := make([]string, 0, len(inputFiles)-len(seen))
		for f := range inputFiles {
			if _, ok := seen[f]; !ok {
				missing = append(missing, f)
			}
		}
		return fmt.Errorf("every section must be either mapped to a chapter or quarantined; these are neither: %s",
			strings.Join(missing, ", "))
	}
	if !contiguousFrom1(nums) {
		return fmt.Errorf("chapter numbers must be unique and run 1..%d with no gaps; got %v", len(nums), nums)
	}
	return nil
}

// readChapterMap decodes the agent's map with the same strictness the validator
// applies, so the post-run harvest can never accept a file validation rejected.
// Sharing one decoder is the point: two readers of one artifact that disagree about
// what is valid is worse than either rule alone.
func readChapterMap(outDir string) (agentChapterMap, error) {
	var m agentChapterMap
	if err := decodeSidecarFile(filepath.Join(outDir, chaptersFileName), &m); err != nil {
		return agentChapterMap{}, fmt.Errorf("out/%s: %w", chaptersFileName, err)
	}
	return m, nil
}

// contiguousFrom1 reports whether nums is exactly 1..len(nums), in any order.
func contiguousFrom1(nums []int) bool {
	seen := make(map[int]bool, len(nums))
	for _, n := range nums {
		if n < 1 || n > len(nums) || seen[n] {
			return false
		}
		seen[n] = true
	}
	return true
}
