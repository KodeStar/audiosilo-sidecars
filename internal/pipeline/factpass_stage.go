package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

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

// knowledgeFinalName is the compact whole-book roster/reveals/threads/ending sheet
// assembled after every independent chunk is complete. It seeds the next book in a
// series and gives synthesis a concise book-level view.
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

// factPassPromptData feeds factpass.md. Field names MUST match the template (rendered
// with missingkey=error).
type factPassPromptData struct {
	Title         string
	From          int
	To            int
	HasInherited  bool
	SpellingSheet string
}

// factAssemblePromptData feeds factpass_assemble.md after every independent chunk
// has completed.
type factAssemblePromptData struct {
	Title         string
	HasInherited  bool
	ChapterCount  int
	SpellingSheet string
}

// factPass is the chunked, resumable fact-extraction pass. Chunks are independent:
// each sees only its chapter range, its spoiler-bounded spelling sheet, and (for a
// later series book) the predecessor's compact final knowledge. They can therefore
// run concurrently and write only chapter-attributed delta facts, instead of
// repeatedly rewriting a growing cumulative sheet. Once every chunk exists, one
// bounded assembly invocation writes knowledge-final.md from the notes only.
func (e *Executor) factPass(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	plan, err := loadChunkPlan(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("fact_pass: load chunk plan (spelling_research must run first): %w", err)
	}
	if len(plan.Chunks) == 0 {
		return scheduler.StageResult{}, fmt.Errorf("fact_pass: chunk plan has no chunks")
	}
	if r.Note != nil {
		r.Note(fmt.Sprintf("fact pass over %s", countNoun(len(plan.Chunks), "chunk")))
	}
	pred, hasCarryover, err := findSeriesPredecessor(ctx, e.db, book)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("fact_pass: find series predecessor: %w", err)
	}

	totalChunks := len(plan.Chunks)
	completed := countCompleteChunks(book.WorkDir, plan)
	if r.Progress != nil {
		r.Progress(completed, totalChunks)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)
	var (
		mu            sync.Mutex
		usageTotal    agentUsage
		needsReview   int
		chunksThisRun int
		firstErr      error
		wg            sync.WaitGroup
	)
	workers := min(e.agentWorkers, totalChunks-completed)
	workerSeconds := make([]float64, workers)
	for workerID := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				usage, chunkReview, chunkErr := e.factPassChunk(ctx, book, r, plan, idx, hasCarryover, pred)
				workerSeconds[workerID] += usage.Seconds
				mu.Lock()
				usageTotal.add(usage.Usage)
				usageTotal.Invocations += usage.Invocations
				usageTotal.Seconds += usage.Seconds
				if chunkErr != nil && firstErr == nil {
					firstErr = chunkErr
					cancel()
				}
				if chunkErr == nil {
					needsReview += chunkReview
					completed++
					chunksThisRun++
					if r.Progress != nil {
						r.Progress(completed, totalChunks)
					}
				}
				mu.Unlock()
			}
		}()
	}
sendJobs:
	for i, chunk := range plan.Chunks {
		if chunkComplete(book.WorkDir, chunk) {
			continue
		}
		select {
		case jobs <- i:
		case <-ctx.Done():
			break sendJobs
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return scheduler.StageResult{}, firstErr
	}
	if err := ctx.Err(); err != nil {
		return scheduler.StageResult{}, err
	}
	// Parallel extraction throughput is governed by the busiest worker lane, not
	// the sum of all concurrent invocation durations. Assembly is serial and is
	// added below.
	productiveSeconds := 0.0
	for _, seconds := range workerSeconds {
		productiveSeconds = max(productiveSeconds, seconds)
	}

	// The assembly is intentionally separate from extraction. It reads only the
	// compact facts (never transcripts), runs once, and is resumable on re-entry.
	assembledThisRun := false
	if !fsutil.IsFile(filepath.Join(book.WorkDir, factsDir, knowledgeFinalName)) {
		usage, aerr := e.assembleFacts(ctx, book, r, plan, hasCarryover, pred)
		usageTotal.add(usage.Usage)
		usageTotal.Invocations += usage.Invocations
		usageTotal.Seconds += usage.Seconds
		productiveSeconds += usage.Seconds
		if aerr != nil {
			return scheduler.StageResult{}, aerr
		}
		assembledThisRun = true
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
	m["parallel_workers"] = workers
	m["assembled"] = assembledThisRun
	// Units are the chunks (re)processed this run - a resume that skipped already-complete
	// chunks records only the ones it actually ran. Seconds use the critical parallel
	// worker lane plus serial assembly, with rate-limit backoff excluded.
	result := scheduler.StageResult{
		Metrics:    metrics(m),
		RateSample: rateSample(chunksThisRun, productiveSeconds),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.FactPass), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// factPassChunk stages and runs one independent chunk. The staged dir contains ONLY
// that range's corrected chapters, its spelling sheet, and optionally the previous
// BOOK's compact final knowledge. It never receives a prior current-book chunk.
func (e *Executor) factPassChunk(ctx context.Context, book store.Book, r scheduler.StageReport, plan chunkPlan, idx int, hasCarryover bool, pred *store.Book) (agentUsage, int, error) {
	chunk := plan.Chunks[idx]

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

	// The predecessor is safe context for every independent chunk. Current-book
	// knowledge is deliberately absent so no chunk depends on another.
	hasInherited := false
	if hasCarryover && pred != nil {
		src := filepath.Join(pred.WorkDir, factsDir, knowledgeFinalName)
		if !fsutil.IsFile(src) {
			return agentUsage{}, 0, fmt.Errorf("fact_pass: predecessor knowledge-final.md missing (%s)", src)
		}
		if err := st.CopyFile(src, knowledgeInheritedName); err != nil {
			return agentUsage{}, 0, fmt.Errorf("fact_pass: stage inherited knowledge sheet: %w", err)
		}
		hasInherited = true
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
		HasInherited:  hasInherited,
		SpellingSheet: sheet,
	}
	// Capture the NEEDS AUDIO REVIEW count from the successful attempt's facts file so
	// no whole-dir re-read is needed for the metric.
	needsReview := 0
	validate := func(_ agent.Result, s *agent.Staging) error {
		n, verr := validateFactPassChunk(s.OutDir(), chunk.From, chunk.To)
		if verr != nil {
			return verr
		}
		needsReview = n
		return nil
	}
	usage, err := e.runAgent(ctx, book, state.FactPass, r, st, "factpass.md", data, false, validate)
	if err != nil {
		return usage, 0, err
	}

	specs := []agent.HarvestSpec{{From: factsChunkName(chunk.From, chunk.To), To: filepath.Join(factsDir, factsChunkName(chunk.From, chunk.To))}}
	if err := agent.Harvest(st, specs); err != nil {
		return usage, 0, fmt.Errorf("fact_pass: harvest chunk %d: %w", idx+1, err)
	}
	return usage, needsReview, nil
}

// validateFactPassChunk checks that the compact facts file exists and carries a
// chapter heading for every chapter in range and none outside it.
func validateFactPassChunk(outDir string, from, to int) (int, error) {
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
	return strings.Count(string(factsData), needsAudioReviewMarker), nil
}

// assembleFacts builds one compact book-level knowledge sheet after all independent
// chunk facts have been harvested. This is the only current-book aggregation call.
func (e *Executor) assembleFacts(ctx context.Context, book store.Book, r scheduler.StageReport, plan chunkPlan, hasCarryover bool, pred *store.Book) (agentUsage, error) {
	st, err := agent.New(book.WorkDir, string(state.FactPass)+"-assemble", e.stageAttempt(ctx, book, state.FactPass))
	if err != nil {
		return agentUsage{}, err
	}
	for _, chunk := range plan.Chunks {
		name := factsChunkName(chunk.From, chunk.To)
		if err := st.CopyFile(filepath.Join(book.WorkDir, factsDir, name), filepath.Join(factsDir, name)); err != nil {
			return agentUsage{}, fmt.Errorf("fact_pass: stage %s for assembly: %w", name, err)
		}
	}
	finalSpellingSheet := spelling.SheetName(plan.Chunks[len(plan.Chunks)-1].To)
	if err := st.CopyFile(filepath.Join(book.WorkDir, factsDir, finalSpellingSheet), finalSpellingSheet); err != nil {
		return agentUsage{}, fmt.Errorf("fact_pass: stage final spelling sheet for assembly: %w", err)
	}
	hasInherited := hasCarryover && pred != nil
	if hasInherited {
		src := filepath.Join(pred.WorkDir, factsDir, knowledgeFinalName)
		if !fsutil.IsFile(src) {
			return agentUsage{}, fmt.Errorf("fact_pass: predecessor knowledge-final.md missing (%s)", src)
		}
		if err := st.CopyFile(src, knowledgeInheritedName); err != nil {
			return agentUsage{}, fmt.Errorf("fact_pass: stage inherited knowledge for assembly: %w", err)
		}
	}
	chapterCount := plan.Chunks[len(plan.Chunks)-1].To
	validate := func(_ agent.Result, s *agent.Staging) error {
		data, rerr := readNonEmptyFile(filepath.Join(s.OutDir(), knowledgeFinalName))
		if rerr != nil {
			return fmt.Errorf("out/%s: %v", knowledgeFinalName, rerr)
		}
		return requireSections(string(data), knowledgeFinalName, "ROSTER", "REVEALS", "THREADS", "ENDING")
	}
	usage, err := e.runAgent(ctx, book, state.FactPass, r, st, "factpass_assemble.md", factAssemblePromptData{
		Title: book.Title, HasInherited: hasInherited, ChapterCount: chapterCount, SpellingSheet: finalSpellingSheet,
	}, false, validate)
	if err != nil {
		return usage, err
	}
	if err := agent.Harvest(st, []agent.HarvestSpec{{From: knowledgeFinalName, To: filepath.Join(factsDir, knowledgeFinalName)}}); err != nil {
		return usage, fmt.Errorf("fact_pass: harvest assembled knowledge: %w", err)
	}
	return usage, nil
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

// chunkComplete is the resume test for independent extraction. Assembly has its own
// knowledge-final.md resume artifact.
func chunkComplete(workDir string, c chunkRange) bool {
	return fsutil.IsFile(filepath.Join(workDir, factsDir, factsChunkName(c.From, c.To)))
}

// countCompleteChunks counts how many plan chunks are already complete (for the resume
// progress baseline).
func countCompleteChunks(workDir string, plan chunkPlan) int {
	n := 0
	for _, c := range plan.Chunks {
		if chunkComplete(workDir, c) {
			n++
		}
	}
	return n
}
