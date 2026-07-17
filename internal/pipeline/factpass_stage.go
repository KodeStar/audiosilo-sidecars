package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// factsDir holds the fact-pass notes and knowledge sheets (shared with the spelling
// sheets GenerateSheets writes).
const factsDir = spelling.FactsDir

// knowledgeFinalName is the whole-book roster/reveals/ENDING sheet the last chunk
// writes: the seed the next book in a series inherits and the synthesis stage reads.
const knowledgeFinalName = "knowledge-final.md"

// knowledgeInheritedName is the staged filename for the SERIES PREDECESSOR's
// knowledge-final.md when it seeds a later book's opening chunk.
const knowledgeInheritedName = "knowledge-inherited.md"

// needsAudioReviewMarker is the fact-pass escape hatch: an unclear word affecting a
// material fact is flagged rather than guessed. The stage counts occurrences into its
// metrics (surfaced, never blocking).
const needsAudioReviewMarker = "NEEDS AUDIO REVIEW"

// factsHeadingRe matches a "## Chapter N" section heading in a facts file.
var factsHeadingRe = regexp.MustCompile(`(?m)^##\s+Chapter\s+(\d+)\b`)

func factsChunkName(from, to int) string { return fmt.Sprintf("facts-ch%d-%d.md", from, to) }
func knowledgeThroughName(to int) string { return fmt.Sprintf("knowledge-through-ch%d.md", to) }

// factPassPromptData feeds factpass.md. Field names MUST match the template (rendered
// with missingkey=error).
type factPassPromptData struct {
	Title         string
	From          int
	To            int
	IsFirstChunk  bool
	IsLastChunk   bool
	HasInherited  bool
	PriorSheet    string
	SpellingSheet string
}

// factPass is the chunked, resumable fact-extraction pass: for each chunk of the plan
// it stages ONLY that chunk's corrected chapters plus the prior cumulative knowledge
// sheet and the chunk's spelling sheet, runs the agent, validates the notes and the
// rolling sheet, and harvests them into facts/. A chunk whose outputs already exist is
// skipped (crash/park resume), so a re-entry only runs the chunks that remain. Usage
// accumulates across chunk invocations.
func (e *Executor) factPass(ctx context.Context, book store.Book, report scheduler.ProgressFunc) (scheduler.StageResult, error) {
	plan, err := loadChunkPlan(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("fact_pass: load chunk plan (spelling_research must run first): %w", err)
	}
	if len(plan.Chunks) == 0 {
		return scheduler.StageResult{}, fmt.Errorf("fact_pass: chunk plan has no chunks")
	}
	pred, hasCarryover, err := findSeriesPredecessor(ctx, e.db, book)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("fact_pass: find series predecessor: %w", err)
	}

	totalChunks := len(plan.Chunks)
	completed := countCompleteChunks(book.WorkDir, plan)
	if report != nil {
		report(completed, totalChunks)
	}

	var usageTotal agentUsage
	needsReview := 0
	chunksThisRun := 0
	for i := range plan.Chunks {
		if err := ctx.Err(); err != nil {
			return scheduler.StageResult{}, err
		}
		isLast := i == len(plan.Chunks)-1
		if chunkComplete(book.WorkDir, plan.Chunks[i], isLast) {
			continue
		}
		usage, chunkReview, cerr := e.factPassChunk(ctx, book, plan, i, hasCarryover, pred)
		usageTotal.add(usage.Usage)
		usageTotal.Invocations += usage.Invocations
		usageTotal.Seconds += usage.Seconds
		needsReview += chunkReview
		if cerr != nil {
			return scheduler.StageResult{}, cerr
		}
		completed++
		chunksThisRun++
		if report != nil {
			report(completed, totalChunks)
		}
	}

	e.accountScratch(ctx, book)
	chapters := 0
	for _, c := range plan.Chunks {
		chapters += c.To - c.From + 1
	}
	m := usageTotal.metricsMap()
	m["chunks"] = totalChunks
	m["chapters"] = chapters
	// Captured from each chunk's validated facts file this run. A mid-stage resume
	// (already-complete chunks skipped) counts only the chunks (re)processed here.
	m["needs_audio_review"] = needsReview
	// Units are the chunks (re)processed this run - a resume that skipped already-complete
	// chunks records only the ones it actually ran - and seconds are the accumulated
	// productive agent time (rate-limit backoff already excluded per chunk).
	result := scheduler.StageResult{
		Metrics:    metrics(m),
		RateSample: rateSample(chunksThisRun, usageTotal.Seconds),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.FactPass), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// factPassChunk stages and runs one chunk. The staged dir contains ONLY the chunk's
// corrected chapters (no chapter outside [from,to]), the chunk's spelling sheet, and
// the prior knowledge sheet (the previous chunk's cumulative sheet, or the series
// predecessor's final sheet renamed knowledge-inherited.md for a later book's opening
// chunk, or nothing for a series opener's first chunk).
func (e *Executor) factPassChunk(ctx context.Context, book store.Book, plan chunkPlan, idx int, hasCarryover bool, pred *store.Book) (agentUsage, int, error) {
	chunk := plan.Chunks[idx]
	isFirst := idx == 0
	isLast := idx == len(plan.Chunks)-1

	st, err := agent.New(book.WorkDir, fmt.Sprintf("%s-c%02d", state.FactPass, idx+1), e.stageAttempt(ctx, book, state.FactPass))
	if err != nil {
		return agentUsage{}, 0, err
	}

	// The chunk's spelling sheet is a hard requirement (correcting produced it).
	sheet := spelling.SheetName(chunk.To)
	sheetSrc := filepath.Join(book.WorkDir, factsDir, sheet)
	if !fsutil.IsFile(sheetSrc) {
		return agentUsage{}, 0, fmt.Errorf("fact_pass: chunk %d spelling sheet %s missing (correcting must run first)", idx+1, filepath.Join(factsDir, sheet))
	}
	if err := st.CopyFile(sheetSrc, sheet); err != nil {
		return agentUsage{}, 0, fmt.Errorf("fact_pass: stage spelling sheet: %w", err)
	}

	// Prior knowledge sheet.
	priorSheet := ""
	hasInherited := false
	switch {
	case isFirst && hasCarryover && pred != nil:
		src := filepath.Join(pred.WorkDir, factsDir, knowledgeFinalName)
		if !fsutil.IsFile(src) {
			return agentUsage{}, 0, fmt.Errorf("fact_pass: predecessor knowledge-final.md missing (%s)", src)
		}
		if err := st.CopyFile(src, knowledgeInheritedName); err != nil {
			return agentUsage{}, 0, fmt.Errorf("fact_pass: stage inherited knowledge sheet: %w", err)
		}
		priorSheet, hasInherited = knowledgeInheritedName, true
	case !isFirst:
		prevTo := plan.Chunks[idx-1].To
		name := knowledgeThroughName(prevTo)
		src := filepath.Join(book.WorkDir, factsDir, name)
		if !fsutil.IsFile(src) {
			return agentUsage{}, 0, fmt.Errorf("fact_pass: prior knowledge sheet %s missing (chunk %d must complete first)", filepath.Join(factsDir, name), idx)
		}
		if err := st.CopyFile(src, name); err != nil {
			return agentUsage{}, 0, fmt.Errorf("fact_pass: stage prior knowledge sheet: %w", err)
		}
		priorSheet = name
	}

	// The chunk's corrected chapters ONLY - the load-bearing spoiler-scope invariant.
	for k := chunk.From; k <= chunk.To; k++ {
		rel := filepath.Join(spelling.CorrectedDir, transcript.TextName(k))
		src := filepath.Join(book.WorkDir, rel)
		if !fsutil.IsFile(src) {
			continue // a genuinely absent chapter file is skipped; never reach outside [from,to]
		}
		if err := st.CopyFile(src, rel); err != nil {
			return agentUsage{}, 0, fmt.Errorf("fact_pass: stage corrected chapter %d: %w", k, err)
		}
	}

	data := factPassPromptData{
		Title:         book.Title,
		From:          chunk.From,
		To:            chunk.To,
		IsFirstChunk:  isFirst,
		IsLastChunk:   isLast,
		HasInherited:  hasInherited,
		PriorSheet:    priorSheet,
		SpellingSheet: sheet,
	}
	// Capture the NEEDS AUDIO REVIEW count from the successful attempt's facts file so
	// no whole-dir re-read is needed for the metric.
	needsReview := 0
	validate := func(_ agent.Result, s *agent.Staging) error {
		n, verr := validateFactPassChunk(s.OutDir(), chunk.From, chunk.To, isLast)
		if verr != nil {
			return verr
		}
		needsReview = n
		return nil
	}
	usage, err := e.runAgent(ctx, book, state.FactPass, st, "factpass.md", data, false, validate)
	if err != nil {
		return usage, 0, err
	}

	specs := []agent.HarvestSpec{
		{From: factsChunkName(chunk.From, chunk.To), To: filepath.Join(factsDir, factsChunkName(chunk.From, chunk.To))},
		{From: knowledgeThroughName(chunk.To), To: filepath.Join(factsDir, knowledgeThroughName(chunk.To))},
	}
	if isLast {
		specs = append(specs, agent.HarvestSpec{From: knowledgeFinalName, To: filepath.Join(factsDir, knowledgeFinalName)})
	}
	if err := agent.Harvest(st, specs); err != nil {
		return usage, 0, fmt.Errorf("fact_pass: harvest chunk %d: %w", idx+1, err)
	}
	return usage, needsReview, nil
}

// validateFactPassChunk checks a chunk's outputs: the facts file exists non-empty and
// carries a "## Chapter k" heading for EVERY k in [from,to] and NONE outside it; the
// cumulative knowledge sheet exists non-empty and names its ROSTER/REVEALS/THREADS
// sections; and on the last chunk knowledge-final.md exists non-empty with an ENDING
// section. It returns the NEEDS AUDIO REVIEW occurrence count in the facts file so the
// stage tallies the metric without a separate re-read.
func validateFactPassChunk(outDir string, from, to int, isLast bool) (int, error) {
	factsName := factsChunkName(from, to)
	factsData, err := readNonEmptyFile(filepath.Join(outDir, factsName))
	if err != nil {
		return 0, fmt.Errorf("out/%s: %v", factsName, err)
	}
	seen := make(map[int]bool)
	for _, m := range factsHeadingRe.FindAllStringSubmatch(string(factsData), -1) {
		n, _ := strconv.Atoi(m[1])
		if n < from || n > to {
			return 0, fmt.Errorf("out/%s has a '## Chapter %d' heading outside the chunk range [%d,%d]", factsName, n, from, to)
		}
		seen[n] = true
	}
	for k := from; k <= to; k++ {
		if !seen[k] {
			return 0, fmt.Errorf("out/%s is missing the '## Chapter %d' heading (chapters %d through %d each need one)", factsName, k, from, to)
		}
	}
	knowName := knowledgeThroughName(to)
	knowData, err := readNonEmptyFile(filepath.Join(outDir, knowName))
	if err != nil {
		return 0, fmt.Errorf("out/%s: %v", knowName, err)
	}
	if err := requireSections(string(knowData), knowName, "ROSTER", "REVEALS", "THREADS"); err != nil {
		return 0, err
	}
	if isLast {
		finalData, err := readNonEmptyFile(filepath.Join(outDir, knowledgeFinalName))
		if err != nil {
			return 0, fmt.Errorf("out/%s: %v", knowledgeFinalName, err)
		}
		if err := requireSections(string(finalData), knowledgeFinalName, "ENDING"); err != nil {
			return 0, err
		}
	}
	return strings.Count(string(factsData), needsAudioReviewMarker), nil
}

// requireSections returns an error naming the first section marker absent from text.
func requireSections(text, name string, sections ...string) error {
	for _, s := range sections {
		if !strings.Contains(text, s) {
			return fmt.Errorf("out/%s is missing the %s section", name, s)
		}
	}
	return nil
}

// readNonEmptyFile reads a file and errors if it is absent or blank.
func readNonEmptyFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is the agent's staged out/ under the work dir
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, fmt.Errorf("file is empty")
	}
	return b, nil
}

// chunkComplete reports whether a chunk's harvested outputs all exist already (the
// resume test): the facts file, the cumulative knowledge sheet, and - for the last
// chunk - knowledge-final.md.
func chunkComplete(workDir string, c chunkRange, isLast bool) bool {
	if !fsutil.IsFile(filepath.Join(workDir, factsDir, factsChunkName(c.From, c.To))) {
		return false
	}
	if !fsutil.IsFile(filepath.Join(workDir, factsDir, knowledgeThroughName(c.To))) {
		return false
	}
	if isLast && !fsutil.IsFile(filepath.Join(workDir, factsDir, knowledgeFinalName)) {
		return false
	}
	return true
}

// countCompleteChunks counts how many plan chunks are already complete (for the resume
// progress baseline).
func countCompleteChunks(workDir string, plan chunkPlan) int {
	n := 0
	for i, c := range plan.Chunks {
		if chunkComplete(workDir, c, i == len(plan.Chunks)-1) {
			n++
		}
	}
	return n
}
