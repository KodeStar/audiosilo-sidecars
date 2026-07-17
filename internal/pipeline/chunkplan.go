package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// chunkPlanFile is the per-book chunk plan the spelling and fact-pass stages share.
const chunkPlanFile = "chunk_plan.json"

// The chunk-planning budget (words). It is the CONTRACT the spelling sheets and the
// fact pass both key on:
//
//   - Chapters are walked in manifest order, accumulating their word counts; a chunk
//     CLOSES the moment its cumulative count reaches chunkWordBudget. This targets the
//     ~20-25k words/chunk the historical hand-run fact pass used.
//   - Every chunk holds at least one chapter, so a single chapter whose own word count
//     is already over budget forms its own chunk.
//   - After planning, if the FINAL chunk holds fewer than chunkMinFinalWords AND there
//     is more than one chunk, it is merged into the previous chunk - so a short
//     trailing chapter does not become a wastefully tiny fact-pass unit.
//
// Word counts prefer transcripts-repaired/<ch>.txt when present, else
// transcripts-text/<ch>.txt (the same per-chapter source preference spelling.Apply
// uses), so the plan matches the text the later stages actually process.
const (
	chunkWordBudget    = 22000
	chunkMinFinalWords = 8000
)

// chunkRange is one fact-pass unit: an inclusive chapter range [From, To].
type chunkRange struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// chunkPlan is chunk_plan.json: the ordered chapter ranges plus their end chapters.
// ChunkEnds[i] == Chunks[i].To by construction; the end list is duplicated so the
// spelling ledger's chunk_ends can be validated against it and the fact-pass prompt
// can render it without re-deriving.
type chunkPlan struct {
	Chunks    []chunkRange `json:"chunks"`
	ChunkEnds []int        `json:"chunk_ends"`
}

// chapterWords pairs a chapter number with its word count, in manifest order.
type chapterWords struct {
	Chapter int
	Words   int
}

// planChunks groups chapters (in the given order) into chunks by the word budget. It
// is pure so the budget and merge rules stay table-testable. A non-empty input yields
// at least one chunk; an empty input yields an empty plan.
func planChunks(chs []chapterWords) chunkPlan {
	var (
		out      []chunkRange
		sums     []int
		curFrom  int
		curWords int
		open     bool
	)
	for _, c := range chs {
		if !open {
			curFrom, curWords, open = c.Chapter, 0, true
		}
		curWords += c.Words
		if curWords >= chunkWordBudget {
			out = append(out, chunkRange{From: curFrom, To: c.Chapter})
			sums = append(sums, curWords)
			open = false
		}
	}
	// A trailing group that never reached the budget (or the whole book when it is
	// under budget) still forms one final chunk.
	if open {
		out = append(out, chunkRange{From: curFrom, To: chs[len(chs)-1].Chapter})
		sums = append(sums, curWords)
	}
	// Merge a too-small final chunk into its predecessor (only when there is one).
	if n := len(out); n > 1 && sums[n-1] < chunkMinFinalWords {
		out[n-2].To = out[n-1].To
		out = out[:n-1]
	}
	p := chunkPlan{Chunks: out}
	p.ChunkEnds = p.ends()
	return p
}

// ends derives the per-chunk end chapters from Chunks (ChunkEnds[i] == Chunks[i].To).
// The derived list is still persisted in chunk_ends so the spelling ledger can be
// validated against it and the fact-pass prompt can render it without re-deriving.
func (p chunkPlan) ends() []int {
	ends := make([]int, len(p.Chunks))
	for i, c := range p.Chunks {
		ends[i] = c.To
	}
	return ends
}

// computeChunkPlan builds the plan for a book: manifest chapter order + per-chapter
// word counts (repaired preferred over raw text). A missing manifest or an empty
// chapter list is a loud, ordered-run error.
func computeChunkPlan(workDir string) (chunkPlan, error) {
	m, err := audio.ReadManifest(workDir)
	if err != nil {
		return chunkPlan{}, fmt.Errorf("chunk plan: read manifest (inspect must run first): %w", err)
	}
	chs := make([]chapterWords, 0, len(m.Chapters))
	for _, ch := range m.Chapters {
		w, werr := chapterWordCount(workDir, ch.Chapter)
		if werr != nil {
			return chunkPlan{}, fmt.Errorf("chunk plan: count chapter %d words: %w", ch.Chapter, werr)
		}
		chs = append(chs, chapterWords{Chapter: ch.Chapter, Words: w})
	}
	if len(chs) == 0 {
		return chunkPlan{}, fmt.Errorf("chunk plan: manifest has no chapters")
	}
	return planChunks(chs), nil
}

// chapterWordCount returns a chapter's word count from its corrected-source text,
// preferring transcripts-repaired/<ch>.txt over transcripts-text/<ch>.txt via the
// shared transcript.ChapterTextPath resolver (the same preference spelling.Apply
// applies). A chapter with no text counts as zero words.
func chapterWordCount(workDir string, chapter int) (int, error) {
	p, ok := transcript.ChapterTextPath(workDir, chapter)
	if !ok {
		return 0, nil
	}
	b, err := os.ReadFile(p) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return 0, err
	}
	return len(strings.Fields(string(b))), nil
}

// writeChunkPlan persists the plan to <workDir>/chunk_plan.json (pretty JSON,
// trailing newline) atomically.
func writeChunkPlan(workDir string, p chunkPlan) error {
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, chunkPlanFile), append(out, '\n'), 0o644)
}

// loadOrComputeChunkPlan returns the persisted chunk plan when chunk_plan.json already
// exists (the source of truth on a re-run), else computes it and persists it once,
// returning the computed plan directly (no write-then-reread round-trip).
func loadOrComputeChunkPlan(workDir string) (chunkPlan, error) {
	if fsutil.IsFile(filepath.Join(workDir, chunkPlanFile)) {
		return loadChunkPlan(workDir)
	}
	plan, err := computeChunkPlan(workDir)
	if err != nil {
		return chunkPlan{}, err
	}
	if err := writeChunkPlan(workDir, plan); err != nil {
		return chunkPlan{}, fmt.Errorf("write chunk plan: %w", err)
	}
	return plan, nil
}

// loadChunkPlan reads <workDir>/chunk_plan.json.
func loadChunkPlan(workDir string) (chunkPlan, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, chunkPlanFile)) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return chunkPlan{}, err
	}
	var p chunkPlan
	if err := json.Unmarshal(raw, &p); err != nil {
		return chunkPlan{}, fmt.Errorf("parse %s: %w", chunkPlanFile, err)
	}
	return p, nil
}

// chunkEndsCSV renders the plan's end chapters as a comma-separated list for the
// spelling/fact-pass prompt templates (which wrap it in brackets).
func (p chunkPlan) chunkEndsCSV() string {
	parts := make([]string, len(p.ChunkEnds))
	for i, e := range p.ChunkEnds {
		parts[i] = strconv.Itoa(e)
	}
	return strings.Join(parts, ", ")
}
