package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/agent/prompts"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// synthesisPromptData feeds synthesis.md. Field names MUST match the template
// (rendered with missingkey=error, so a drift fails loudly at render time).
type synthesisPromptData struct {
	Title          string
	Authors        string
	Series         string
	SeriesPos      string
	ChapterCount   int
	WorkSlug       string
	IsSeriesOpener bool
	VerifiedLedger string
}

// synthesize is the NOTES-ONLY boundary: the agent authors the CC BY-SA sidecars
// from the private fact notes alone. Its staged dir deliberately contains NO
// transcripts, manifest, or QA artifacts (the tested invariant that keeps spoiler
// bounds auditable and verbatim overlap impossible by construction) - only the
// authoring contract and facts/. Outputs are validated against the full structural
// contract (errors only; the missing-book-2-recap warning is tolerated here and
// surfaced by validating) and harvested under sidecars/.
func (e *Executor) synthesize(ctx context.Context, book store.Book, report scheduler.ProgressFunc) (scheduler.StageResult, error) {
	if report != nil {
		report(0, 1)
	}
	manifest, seriesOpener, ledger, err := e.sidecarStageInputs(ctx, book)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("synthesizing: %w", err)
	}

	st, err := agent.New(book.WorkDir, string(state.Synthesizing), e.stageAttempt(ctx, book, state.Synthesizing))
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if err := stageAuthoring(st); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("synthesizing: stage authoring.md: %w", err)
	}
	facts := factsDirPath(book.WorkDir)
	if !isDir(facts) {
		return scheduler.StageResult{}, fmt.Errorf("synthesizing: facts/ missing (fact_pass must run first)")
	}
	if err := st.CopyDir(facts, factsDir, mdFilter); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("synthesizing: stage facts: %w", err)
	}

	// Capture the validated sidecars' counts from the successful attempt so the metrics
	// tally needs no post-harvest reload (harvest is a straight copy of these files).
	var cards, recaps, warnings int
	validate := func(_ agent.Result, s *agent.Staging) error {
		chars, recs, warns, verr := loadOutSidecars(s.OutDir(), manifest.ChapterCount, seriesOpener)
		if verr != nil {
			return verr
		}
		cards, recaps, warnings = len(chars.Characters), len(recs.Recaps), len(warns)
		return nil
	}
	data := synthesisPromptData{
		Title:          book.Title,
		Authors:        authors(book),
		Series:         book.Series,
		SeriesPos:      book.SeriesPos,
		ChapterCount:   manifest.ChapterCount,
		WorkSlug:       workSlug(book),
		IsSeriesOpener: seriesOpener,
		VerifiedLedger: ledger,
	}
	usage, err := e.runAgent(ctx, book, state.Synthesizing, st, "synthesis.md", data, false, validate)
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if err := harvestSidecars(st); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("synthesizing: harvest sidecars: %w", err)
	}
	if report != nil {
		report(1, 1)
	}

	m := usage.metricsMap()
	m["cards"] = cards
	m["recaps"] = recaps
	m["warnings"] = warnings
	result := scheduler.StageResult{Metrics: metrics(m)}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Synthesizing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// sidecarStageInputs resolves the inputs the sidecar stages share: the manifest (for
// ChapterCount), whether the book opens its series (the chapter-0 recap rule), and the
// verified-spelling ledger table for the prompts. validating uses only the manifest +
// opener and ignores the ledger. Errors are unprefixed; each stage wraps with its own
// name.
func (e *Executor) sidecarStageInputs(ctx context.Context, book store.Book) (audio.Manifest, bool, string, error) {
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return audio.Manifest{}, false, "", fmt.Errorf("read manifest (inspect must run first): %w", err)
	}
	seriesOpener, err := e.isSeriesOpener(ctx, book)
	if err != nil {
		return audio.Manifest{}, false, "", fmt.Errorf("series predecessor lookup: %w", err)
	}
	ledger, err := verifiedLedgerTable(book.WorkDir)
	if err != nil {
		return audio.Manifest{}, false, "", fmt.Errorf("verified ledger: %w", err)
	}
	return manifest, seriesOpener, ledger, nil
}

// authoringName is the vendored authoring contract staged into every notes-only /
// audit / fix dir.
const authoringName = "authoring.md"

// stageAuthoring writes the vendored authoring.md (the CC BY-SA authoring contract)
// into the staged dir. It has no template actions, so Render returns it verbatim.
func stageAuthoring(st *agent.Staging) error {
	text, err := prompts.Render(authoringName, nil)
	if err != nil {
		return err
	}
	return st.WriteFile(authoringName, []byte(text))
}

// loadOutSidecars decodes and validates the agent's out/characters.json +
// out/recaps.json. Validation ERRORS fail (retry); the returned warns (the missing
// book-2 series recap) are tolerated by the synthesis/fix validators. It returns the
// decoded files and warnings so a caller can reuse them without re-validating.
func loadOutSidecars(outDir string, chapterCount int, seriesOpener bool) (*model.Characters, *model.Recaps, []string, error) {
	var chars model.Characters
	if err := decodeSidecarFile(filepath.Join(outDir, charactersFileName), &chars); err != nil {
		return nil, nil, nil, fmt.Errorf("out/%s: %v", charactersFileName, err)
	}
	var recs model.Recaps
	if err := decodeSidecarFile(filepath.Join(outDir, recapsFileName), &recs); err != nil {
		return nil, nil, nil, fmt.Errorf("out/%s: %v", recapsFileName, err)
	}
	errs, warns := validateSidecars(&chars, &recs, chapterCount, seriesOpener)
	if len(errs) > 0 {
		return nil, nil, nil, fmt.Errorf("sidecar validation failed: %s", joinFindings(errs))
	}
	return &chars, &recs, warns, nil
}

// loadWorkSidecars decodes the harvested sidecars/characters.json + sidecars/recaps.json
// from the work dir (no validation - the caller computes counts/findings).
func loadWorkSidecars(workDir string) (*model.Characters, *model.Recaps, error) {
	var chars model.Characters
	if err := decodeSidecarFile(filepath.Join(workDir, sidecarsDir, charactersFileName), &chars); err != nil {
		return nil, nil, err
	}
	var recs model.Recaps
	if err := decodeSidecarFile(filepath.Join(workDir, sidecarsDir, recapsFileName), &recs); err != nil {
		return nil, nil, err
	}
	return &chars, &recs, nil
}

// harvestSidecars moves out/characters.json + out/recaps.json into sidecars/.
func harvestSidecars(st *agent.Staging) error {
	return agent.Harvest(st, []agent.HarvestSpec{
		{From: charactersFileName, To: filepath.Join(sidecarsDir, charactersFileName)},
		{From: recapsFileName, To: filepath.Join(sidecarsDir, recapsFileName)},
	})
}

// joinFindings renders a bounded, newline-free summary of validation findings for a
// retry prompt / error message.
func joinFindings(findings []string) string {
	const max = 20
	if len(findings) > max {
		findings = append(findings[:max:max], fmt.Sprintf("... and %d more", len(findings)-max))
	}
	return strings.Join(findings, "; ")
}
