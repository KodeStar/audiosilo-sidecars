package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
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
// with missingkey=error). ChunkNote is the fact-pass CHUNK edge note (file-numbered headings;
// spoken numbers only in fact text), NOT the generic EdgeNote - a chunk keys its headings to
// audio-file numbers, so it must never be told to renumber to spoken chapters.
type factPassPromptData struct {
	Title        string
	From         int
	To           int
	HasInherited bool
	ChunkNote    string
	// SpellingSheet is empty for an ebook, whose spellings are exact by
	// construction, which drops the sheet's bullet and its ASR-trust rules.
	SpellingSheet string
	// IsEbook swaps the source-specific guidance ONLY. Everything that carries the
	// spoiler boundary - the chapter range, the heading contract, chapter
	// attribution, own-words - is outside the conditional and identical for both
	// kinds, which prompts_test.go asserts so the branches cannot drift.
	IsEbook bool
	// TextDir is the staged directory holding this chunk's chapters.
	TextDir string
}

// factAssemblePromptData feeds factpass_assemble.md after every independent chunk
// has completed. AssembleNote is the fact-pass ASSEMBLE edge note - the ONE renumbering
// boundary, carrying the concrete file->spoken mapping when there is a leading exclusion.
type factAssemblePromptData struct {
	Title         string
	HasInherited  bool
	ChapterCount  int
	AssembleNote  string
	SpellingSheet string
	IsEbook       bool
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
		return scheduler.StageResult{}, fmt.Errorf("fact_pass: load chunk plan (%s must run first): %w", chunkPlanAuthor(book), err)
	}
	if len(plan.Chunks) == 0 {
		return scheduler.StageResult{}, fmt.Errorf("fact_pass: chunk plan has no chunks")
	}
	// Classify the book's edge chapters (a non-narrative intro/credits file on a
	// files-style book) so the chunk agents get the EdgeNote and the assembler the
	// LOGICAL story-chapter count. A normal book yields an empty note and the raw count.
	class, err := classifyBookEdges(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("fact_pass: %w", err)
	}
	noteEdgeExclusions(r, class)
	// Drop facts derived from a DIFFERENT chapter universe before any of them is
	// reused. A harvested chunk is named only for its chapter range, and facts/ is
	// durable - so when a book is re-extracted (a purge rewinds to extracting, and the
	// mapping agent is not deterministic) a new numbering whose chunk boundaries
	// happen to land the same way would resume straight onto the old chunk's file.
	// Every fact in it is then attributed to the wrong chapter, which is the published
	// spoiler position, and nothing downstream re-derives it: validateSidecars checks
	// the chapter COUNT, the n-gram check looks for verbatim overlap, and the auditor
	// sees a set that is internally consistent.
	dropped, err := discardStaleFacts(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("fact_pass: %w", err)
	}
	if dropped && r.Note != nil {
		r.Note("the chapter universe changed since the last fact pass, so previously extracted facts were discarded rather than re-attributed to different chapters")
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
				usage, chunkReview, chunkErr := e.factPassChunk(ctx, book, r, plan, idx, hasCarryover, pred, class)
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
	// Sum productive invocation time so the learned seconds-per-chunk rate is
	// independent of how those invocations happened to be distributed over workers.
	extractionInvocationSeconds := 0.0
	for _, seconds := range workerSeconds {
		extractionInvocationSeconds += seconds
	}

	// The assembly is intentionally separate from extraction. It reads only the
	// compact facts (never transcripts), runs once, and is resumable on re-entry.
	assembledThisRun := false
	if !fsutil.IsFile(filepath.Join(book.WorkDir, factsDir, knowledgeFinalName)) {
		usage, aerr := e.assembleFacts(ctx, book, r, plan, hasCarryover, pred, class)
		usageTotal.add(usage.Usage)
		usageTotal.Invocations += usage.Invocations
		usageTotal.Seconds += usage.Seconds
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
	// chunks records only the ones it actually ran.
	result := scheduler.StageResult{
		Metrics: metrics(m),
		// Learn seconds per chunk invocation, independent of the configured fan-out.
		// Serial assembly is deliberately excluded from this topology-neutral rate.
		RateSample: rateSample(chunksThisRun, extractionInvocationSeconds),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.FactPass), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// factPassChunk stages and runs one independent chunk. The staged dir contains ONLY
// that range's corrected chapters, its spelling sheet, and optionally the previous
// BOOK's compact final knowledge. It never receives a prior current-book chunk.
func (e *Executor) factPassChunk(ctx context.Context, book store.Book, r scheduler.StageReport, plan chunkPlan, idx int, hasCarryover bool, pred *store.Book, class edgeClassification) (agentUsage, int, error) {
	chunk := plan.Chunks[idx]

	st, err := agent.New(book.WorkDir, fmt.Sprintf("%s-c%02d", state.FactPass, idx+1), e.stageAttempt(ctx, book, state.FactPass))
	if err != nil {
		return agentUsage{}, 0, err
	}

	isEbook := state.ParseKind(book.Kind) == state.KindEbook

	// The spoiler-bounded spelling sheet is what lets the agent trust a proper noun
	// the ASR may have mangled, so on the audio path it is a hard requirement that
	// correcting produced. An ebook has exact spellings by construction and never
	// runs the spelling stages, so there is no sheet and none is wanted.
	sheet := ""
	if !isEbook {
		sheet = spelling.SheetName(chunk.To)
		sheetSrc := filepath.Join(book.WorkDir, factsDir, sheet)
		if !fsutil.IsFile(sheetSrc) {
			return agentUsage{}, 0, fmt.Errorf("fact_pass: chunk %d spelling sheet %s missing (correcting must run first)", idx+1, filepath.Join(factsDir, sheet))
		}
		if err := st.CopyFile(sheetSrc, sheet); err != nil {
			return agentUsage{}, 0, fmt.Errorf("fact_pass: stage spelling sheet: %w", err)
		}
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

	// The chunk's chapters ONLY - the load-bearing spoiler-scope invariant. The
	// directory differs by kind but the [from,to] bound does not.
	textDir := chapterTextDir(book)
	for k := chunk.From; k <= chunk.To; k++ {
		rel := filepath.Join(textDir, transcript.TextName(k))
		src := filepath.Join(book.WorkDir, rel)
		if !fsutil.IsFile(src) {
			// On the ebook path this layer is exactly what a scratch purge reclaims, so
			// a missing chapter is never benign: it means the prose was deleted and the
			// stage was re-entered without re-extracting. Skipping would hand the agent
			// a chunk holding a prompt and nothing to read, and validateFactPassChunk
			// only requires one "## Chapter k" heading per chapter in range - which a
			// compliant agent emits with nothing in front of it. The fabricated facts
			// would then be harvested, and the failure would surface two stages later
			// as an empty n-gram check, after a full fact-pass and synthesis spend.
			if isEbook {
				return agentUsage{}, 0, fmt.Errorf(
					"fact_pass: chapter %d text is missing from %s; the book text was reclaimed, so re-run extracting (Retry after a purge) rather than authoring facts from nothing",
					k, textDir)
			}
			continue // a genuinely absent chapter file is skipped; never reach outside [from,to]
		}
		if err := st.CopyFile(src, rel); err != nil {
			return agentUsage{}, 0, fmt.Errorf("fact_pass: stage chapter %d text: %w", k, err)
		}
	}

	data := factPassPromptData{
		Title:         book.Title,
		From:          chunk.From,
		To:            chunk.To,
		HasInherited:  hasInherited,
		ChunkNote:     class.ChunkNote,
		IsEbook:       isEbook,
		TextDir:       textDir,
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
func (e *Executor) assembleFacts(ctx context.Context, book store.Book, r scheduler.StageReport, plan chunkPlan, hasCarryover bool, pred *store.Book, class edgeClassification) (agentUsage, error) {
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
	finalSpellingSheet := ""
	if state.ParseKind(book.Kind) != state.KindEbook {
		finalSpellingSheet = spelling.SheetName(plan.Chunks[len(plan.Chunks)-1].To)
		if err := st.CopyFile(filepath.Join(book.WorkDir, factsDir, finalSpellingSheet), finalSpellingSheet); err != nil {
			return agentUsage{}, fmt.Errorf("fact_pass: stage final spelling sheet for assembly: %w", err)
		}
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
	validate := func(_ agent.Result, s *agent.Staging) error {
		data, rerr := readNonEmptyFile(filepath.Join(s.OutDir(), knowledgeFinalName))
		if rerr != nil {
			return fmt.Errorf("out/%s: %v", knowledgeFinalName, rerr)
		}
		return requireSections(string(data), knowledgeFinalName, "ROSTER", "REVEALS", "THREADS", "ENDING")
	}
	usage, err := e.runAgent(ctx, book, state.FactPass, r, st, "factpass_assemble.md", factAssemblePromptData{
		Title: book.Title, HasInherited: hasInherited, ChapterCount: class.LogicalCount, AssembleNote: class.AssembleNote, SpellingSheet: finalSpellingSheet,
		IsEbook: state.ParseKind(book.Kind) == state.KindEbook,
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

// universeStampName records which chapter universe the facts beside it were
// extracted from.
const universeStampName = ".universe"

// discardStaleFacts removes harvested facts that belong to a superseded chapter
// numbering, reporting whether it removed any.
//
// A fact file is named for its chapter RANGE alone (facts-ch1-9.md), and facts/ is
// durable - it survives a scratch purge and is never reclaimed. So a re-derived
// universe whose chunk boundaries coincide with the old one resumes onto the previous
// numbering's files, and every fact in them is silently re-attributed to a chapter it
// did not come from. The stamp is what makes "same range" mean "same chapters".
//
// It fingerprints the manifest's chapter list rather than the chunk plan, because the
// plan is derived from word budgets and can be identical across two different
// numberings - which is exactly the case that would otherwise slip through.
func discardStaleFacts(workDir string) (bool, error) {
	m, err := audio.ReadManifest(workDir)
	if err != nil {
		return false, fmt.Errorf("read manifest for the facts universe stamp: %w", err)
	}
	stamp := universeStamp(m)
	dir := filepath.Join(workDir, factsDir)
	path := filepath.Join(dir, universeStampName)

	prev, rerr := os.ReadFile(path) //nolint:gosec // path derives from the book's work dir
	switch {
	case rerr == nil && string(prev) == stamp:
		return false, nil
	case rerr != nil && !os.IsNotExist(rerr):
		return false, rerr
	}

	// Only a MISMATCH discards. An unstamped directory is adopted: it belongs to a
	// book already in flight when this guard shipped, discarding would re-charge it
	// for the most expensive stage in the pipeline, and the path where a re-derived
	// numbering is routine - ebook, where a purge forces a re-extract and the mapping
	// agent is not deterministic - carries the stamp from its first run, so no ebook
	// facts can be unstamped in the first place.
	removed := false
	if rerr == nil {
		var err error
		if removed, err = removeFactArtifacts(dir); err != nil {
			return false, err
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return false, err
	}
	return removed, fsutil.WriteFileAtomic(path, []byte(stamp), 0o644)
}

// removeFactArtifacts deletes the fact pass's own output, reporting whether any was
// there.
//
// It removes ONLY what the fact pass wrote. facts/ is shared with the spelling sheets
// GenerateSheets produced in an earlier stage, and factPassChunk hard-requires them -
// so clearing the directory wholesale would strip an input the stage cannot run
// without, on every audio book.
func removeFactArtifacts(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	removed := false
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "facts-ch") && name != knowledgeFinalName {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return removed, err
		}
		removed = true
	}
	return removed, nil
}

// universeStamp fingerprints the chapter universe: the numbers, the sections that
// make each one up, and their sizes. Any of those moving changes which prose sits
// under which chapter number.
func universeStamp(m audio.Manifest) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1\n%s\n%d\n", m.Style, len(m.Chapters))
	for _, c := range m.Chapters {
		fmt.Fprintf(h, "%d\t%d\t%d\t%d\t%s\n", c.Chapter, c.Words, int(c.Start), int(c.End), c.FilePath)
	}
	return hex.EncodeToString(h.Sum(nil))
}
