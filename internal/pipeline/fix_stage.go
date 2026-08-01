package pipeline

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// fixPromptData feeds fix.md.
type fixPromptData struct {
	Title          string
	ChapterCount   int
	EdgeNote       string
	IsSeriesOpener bool
	// HasSeriesPrior reports that series-previously.md was staged: the ONE source the
	// fixer may draw prior-book content from when an audit finding asks for the
	// chapter-0 "previously" recap.
	HasSeriesPrior bool
	VerifiedLedger string
}

// fixSidecars applies the auditor's findings: the agent emits COMPLETE replacement
// sidecars, validated against the same structural contract as synthesis, then
// re-enters validating (advance clears the validating sentinel so it re-runs). The
// fix-loop cap is scheduler-owned (CountStageSuccesses("fixing") >= 3 parks at
// auditing), so this stage does no round accounting of its own.
func (e *Executor) fixSidecars(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	in, err := e.sidecarStageInputs(ctx, book)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("fixing: %w", err)
	}
	noteEdgeExclusions(r, in.class)

	st, err := agent.New(book.WorkDir, string(state.Fixing), e.stageAttempt(ctx, book, state.Fixing))
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if err := stageFixInputs(st, book.WorkDir, in.prior); err != nil {
		return scheduler.StageResult{}, err
	}

	validate := func(_ agent.Result, s *agent.Staging) error {
		_, _, _, verr := loadOutSidecars(s.OutDir(), in.class.LogicalCount, in.seriesOpener)
		return verr
	}
	data := fixPromptData{
		Title:          book.Title,
		ChapterCount:   in.class.LogicalCount,
		EdgeNote:       in.class.EdgeNote,
		IsSeriesOpener: in.seriesOpener,
		HasSeriesPrior: in.prior.present(),
		VerifiedLedger: in.ledger,
	}
	usage, err := e.runAgent(ctx, book, state.Fixing, r, st, "fix.md", data, false, validate)
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if err := harvestSidecars(st); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("fixing: harvest sidecars: %w", err)
	}
	if r.Progress != nil {
		r.Progress(1, 1)
	}
	result := scheduler.StageResult{Metrics: metrics(usage.metricsMap()), RateSample: usage.rateSample()}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Fixing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// stageFixInputs copies the fixer's input set: the authoring contract, the current
// sidecars, the audit findings, the validation report, the fact notes (the only source
// the fixer may draw new wording from), and, for a book-2+ whose predecessor is only
// covered upstream, that predecessor's published recap material.
func stageFixInputs(st *agent.Staging, workDir string, prior seriesPrior) error {
	if err := stageAuthoring(st); err != nil {
		return fmt.Errorf("fixing: stage authoring.md: %w", err)
	}
	if err := stageSeriesPrior(st, prior); err != nil {
		return fmt.Errorf("fixing: %w", err)
	}
	if err := stageSidecars(st, workDir); err != nil {
		return err
	}
	for _, name := range []string{auditReportName, validationReportName} {
		if err := st.CopyFile(filepath.Join(workDir, name), name); err != nil {
			return fmt.Errorf("fixing: stage %s: %w", name, err)
		}
	}
	facts := factsDirPath(workDir)
	if !isDir(facts) {
		return fmt.Errorf("fixing: facts/ missing (fact_pass must run first)")
	}
	if err := st.CopyDir(facts, factsDir, mdFilter); err != nil {
		return fmt.Errorf("fixing: stage facts: %w", err)
	}
	return nil
}
