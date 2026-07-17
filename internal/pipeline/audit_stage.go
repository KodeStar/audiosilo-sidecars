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

// auditPromptData feeds audit.md.
type auditPromptData struct {
	Title          string
	ChapterCount   int
	IsSeriesOpener bool
	VerifiedLedger string
}

// audit runs the independent adversarial auditor over the (canonical) sidecars. It
// stages a FRESH dir per attempt (staging always rebuilds) holding the authoring
// contract, the sidecars, the mechanical validation report, and the fact notes. The
// pass/fail decision is re-derived IN CODE (effectivePass): the agent's Pass is
// honored only when it reports no BLOCKER/FIX finding AND validation_report is clean,
// so an over-optimistic Pass=true is overridden to false and drives another fix
// round. The fix-loop cap is scheduler-owned (CountStageSuccesses("fixing")).
func (e *Executor) audit(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	manifest, seriesOpener, ledger, err := e.sidecarStageInputs(ctx, book)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("auditing: %w", err)
	}

	st, err := agent.New(book.WorkDir, string(state.Auditing), e.stageAttempt(ctx, book, state.Auditing))
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if err := stageAuditInputs(st, book.WorkDir); err != nil {
		return scheduler.StageResult{}, err
	}

	// Capture the parsed report from the successful attempt so no post-harvest reload is
	// needed (harvest is a straight copy of this file).
	var rep AuditReport
	validate := func(_ agent.Result, s *agent.Staging) error {
		r, verr := loadAuditReport(s.OutDir())
		if verr != nil {
			return verr
		}
		rep = r
		return nil
	}
	data := auditPromptData{
		Title:          book.Title,
		ChapterCount:   manifest.ChapterCount,
		IsSeriesOpener: seriesOpener,
		VerifiedLedger: ledger,
	}
	usage, err := e.runAgent(ctx, book, state.Auditing, r, st, "audit.md", data, false, validate)
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if err := agent.Harvest(st, []agent.HarvestSpec{{From: auditReportName, To: auditReportName}}); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("auditing: harvest audit.json: %w", err)
	}

	valRep, err := loadValidationReport(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("auditing: read validation report (validating must run first): %w", err)
	}
	blocker, fix, nit := rep.counts()
	effectivePass := rep.Pass && blocker == 0 && fix == 0 && valRep.Clean
	if rep.Pass && !effectivePass {
		e.log.Warn("auditing: overriding agent pass=true (inconsistent with findings/validation)",
			"book", book.ID, "blocker", blocker, "fix", fix, "validation_clean", valRep.Clean)
	}
	if r.Progress != nil {
		r.Progress(1, 1)
	}
	m := usage.metricsMap()
	m["blocker"] = blocker
	m["fix"] = fix
	m["nit"] = nit
	m["pass"] = effectivePass
	result := scheduler.StageResult{
		AuditPassed: effectivePass,
		Metrics:     metrics(m),
		RateSample:  usage.rateSample(),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Auditing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// stageAuditInputs copies the auditor's fixed input set into the staged dir: the
// authoring contract, the sidecars under audit, the mechanical validation report, and
// the fact notes.
func stageAuditInputs(st *agent.Staging, workDir string) error {
	if err := stageAuthoring(st); err != nil {
		return fmt.Errorf("auditing: stage authoring.md: %w", err)
	}
	if err := stageSidecars(st, workDir); err != nil {
		return err
	}
	if err := st.CopyFile(filepath.Join(workDir, validationReportName), validationReportName); err != nil {
		return fmt.Errorf("auditing: stage %s: %w", validationReportName, err)
	}
	facts := factsDirPath(workDir)
	if !isDir(facts) {
		return fmt.Errorf("auditing: facts/ missing (fact_pass must run first)")
	}
	if err := st.CopyDir(facts, factsDir, mdFilter); err != nil {
		return fmt.Errorf("auditing: stage facts: %w", err)
	}
	return nil
}

// stageSidecars copies sidecars/characters.json + sidecars/recaps.json into the
// staged dir under sidecars/ (shared by the audit and fix stages).
func stageSidecars(st *agent.Staging, workDir string) error {
	for _, name := range []string{charactersFileName, recapsFileName} {
		rel := filepath.Join(sidecarsDir, name)
		if err := st.CopyFile(filepath.Join(workDir, rel), rel); err != nil {
			return fmt.Errorf("stage %s: %w", rel, err)
		}
	}
	return nil
}

// loadAuditReport parses out/audit.json from an arbitrary dir and validates its
// severities are in the enum.
func loadAuditReport(dir string) (AuditReport, error) {
	return loadAuditReportFile(filepath.Join(dir, auditReportName))
}

// loadAuditReportFile parses an audit.json at an exact path and validates severities.
func loadAuditReportFile(path string) (AuditReport, error) {
	var rep AuditReport
	if err := decodeSidecarFile(path, &rep); err != nil {
		return rep, fmt.Errorf("%s: %v", auditReportName, err)
	}
	for i, f := range rep.Findings {
		switch f.Severity {
		case SeverityBlocker, SeverityFix, SeverityNit:
		default:
			return rep, fmt.Errorf("%s: findings[%d].severity %q is not one of BLOCKER/FIX/NIT", auditReportName, i, f.Severity)
		}
	}
	return rep, nil
}
