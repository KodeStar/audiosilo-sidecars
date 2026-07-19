package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
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

// auditVerifyPromptData feeds audit_verify.md on the bounded convergence path.
// Unlike a fresh adversarial audit, this pass checks only the exact FIX findings
// accepted for one final correction round.
type auditVerifyPromptData struct {
	Title        string
	ChapterCount int
	Round        int
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

	// Round accounting + acceptance re-entry (db-backed only; without a db the stage is a
	// single-shot audit with no trajectory, matching the pre-change behaviour). round is
	// this run's 1-based audit round; CountStageSuccesses counts COMPLETED rounds (the
	// current run is still open), mirroring qaAdjudicate.
	round := 1
	if e.db != nil {
		done, cerr := e.db.CountStageSuccesses(ctx, book.ID, string(state.Auditing))
		if cerr != nil {
			return scheduler.StageResult{}, fmt.Errorf("auditing: count rounds: %w", cerr)
		}
		if done == 0 {
			// Fresh admit / Retry / purge-rewind: never inherit a prior life's history or a
			// stale acceptance marker (mirrors qaAdjudicate's done==0 reset).
			removeAuditTrajectory(book.WorkDir)
		}
		round = done + 1
		// Acceptance re-entry: a prior round converged and the final fixing round has
		// re-validated. Run a focused verifier over those exact fixes; if mechanical
		// validation is dirty, drop the marker and fall through to a fresh audit.
		if res, handled, aerr := e.auditReentryAccept(ctx, book, r); handled || aerr != nil {
			return res, aerr
		}
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

	// Trajectory-aware acceptance (db-backed): terminate a converging loop by ACCEPTING,
	// never by shipping known defects. Record this round in the history, then decide.
	if e.db != nil && !effectivePass {
		prev := loadAuditRounds(book.WorkDir) // BEFORE appending: the previous round is last
		prevBlocker, prevFix, prevOK := 0, 0, false
		if n := len(prev); n > 0 {
			prevBlocker, prevFix, prevOK = prev[n-1].Blocker, prev[n-1].Fix, true
		}
		if aerr := appendAuditRound(book.WorkDir, auditRound{Round: round, Blocker: blocker, Fix: fix, Nit: nit}); aerr != nil {
			return scheduler.StageResult{}, fmt.Errorf("auditing: record round history: %w", aerr)
		}
		fixesDone, cerr := e.db.CountStageSuccesses(ctx, book.ID, string(state.Fixing))
		if cerr != nil {
			return scheduler.StageResult{}, fmt.Errorf("auditing: count fix rounds: %w", cerr)
		}
		if acceptTrajectory(round, blocker, fix, prevBlocker, prevFix, prevOK, fixesDone, valRep.Clean, state.MaxFixAttempts) {
			acc := auditAccepted{Round: round, Fix: fix, Nit: nit, Findings: nonBlockerFindings(rep)}
			if werr := writeAuditAccepted(book.WorkDir, acc); werr != nil {
				return scheduler.StageResult{}, fmt.Errorf("auditing: write acceptance marker: %w", werr)
			}
			m["accepting"] = true
			result.Metrics = metrics(m)
			if r.Note != nil {
				r.Note(fmt.Sprintf("audit converged after %d rounds (fix trajectory %s); applying final fixes and accepting with %d residual nit(s) recorded",
					round, fixTrajectory(loadAuditRounds(book.WorkDir)), nit))
			}
		} else {
			// Not accepted: attach the trajectory so advance()'s fix-loop-exhausted park
			// (if the budget is now spent) surfaces WHY it did not converge. Harmless when
			// the loop instead continues to a fix round (advance ignores it).
			result.ParkMessage = fixLoopParkMessage(loadAuditRounds(book.WorkDir), fixesDone)
		}
	}

	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Auditing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// auditReentryAccept handles the entry after a converging audit's final fixing round.
// Mechanical validation alone cannot prove that semantic FIX findings were applied,
// so this path now invokes a focused verifier over the exact accepted findings. A
// clean targeted report passes; an unresolved item removes the marker and re-enters
// the ordinary fix/audit path rather than silently shipping a known defect.
func (e *Executor) auditReentryAccept(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, bool, error) {
	acc, ok := loadAuditAccepted(book.WorkDir)
	if !ok {
		return scheduler.StageResult{}, false, nil
	}
	valRep, err := loadValidationReport(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, false, fmt.Errorf("auditing: read validation report (validating must run first): %w", err)
	}
	if !valRep.Clean {
		// The final fix broke canonical form (rare): drop the marker and run a real round.
		_ = os.Remove(auditAcceptedPath(book.WorkDir))
		return scheduler.StageResult{}, false, nil
	}
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, false, fmt.Errorf("auditing: verification read manifest: %w", err)
	}
	st, err := agent.New(book.WorkDir, string(state.Auditing)+"-verify", e.stageAttempt(ctx, book, state.Auditing))
	if err != nil {
		return scheduler.StageResult{}, false, err
	}
	if err := stageAuditInputs(st, book.WorkDir); err != nil {
		return scheduler.StageResult{}, false, err
	}
	if err := st.CopyFile(auditAcceptedPath(book.WorkDir), scheduler.AuditAcceptedFile); err != nil {
		return scheduler.StageResult{}, false, fmt.Errorf("auditing: stage accepted findings: %w", err)
	}
	var rep AuditReport
	validate := func(_ agent.Result, s *agent.Staging) error {
		parsed, verr := loadAuditReport(s.OutDir())
		if verr != nil {
			return verr
		}
		rep = parsed
		return nil
	}
	usage, err := e.runAgent(ctx, book, state.Auditing, r, st, "audit_verify.md", auditVerifyPromptData{
		Title: book.Title, ChapterCount: manifest.ChapterCount, Round: acc.Round,
	}, false, validate)
	if err != nil {
		return scheduler.StageResult{}, true, err
	}
	if err := agent.Harvest(st, []agent.HarvestSpec{{From: auditReportName, To: auditReportName}}); err != nil {
		return scheduler.StageResult{}, true, fmt.Errorf("auditing: harvest verification audit.json: %w", err)
	}
	blocker, fix, nit := rep.counts()
	passed := rep.Pass && blocker == 0 && fix == 0
	m := usage.metricsMap()
	m["pass"] = passed
	m["targeted_verification"] = true
	m["accepted_after_rounds"] = acc.Round
	m["residual_nits"] = acc.Nit
	m["blocker"] = blocker
	m["fix"] = fix
	m["nit"] = nit
	result := scheduler.StageResult{AuditPassed: passed, Metrics: metrics(m), RateSample: usage.rateSample()}
	if !passed {
		_ = os.Remove(auditAcceptedPath(book.WorkDir))
		if r.Note != nil {
			r.Note(fmt.Sprintf("audit verification found %d unresolved blocker(s) and %d fix(es); continuing the normal correction path", blocker, fix))
		}
	} else if r.Note != nil {
		r.Note(fmt.Sprintf("audit accepted after targeted verification (converged round %d; %d residual nit(s) recorded)", acc.Round, acc.Nit))
	}
	if r.Progress != nil {
		r.Progress(1, 1)
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Auditing), result); err != nil {
		return scheduler.StageResult{}, true, err
	}
	return result, true, nil
}

// nonBlockerFindings returns the report's FIX and NIT findings (a BLOCKER never reaches
// the acceptance path), recorded in the acceptance marker so it shows exactly what was
// accepted with the sidecars.
func nonBlockerFindings(rep AuditReport) []AuditFinding {
	out := make([]AuditFinding, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		if f.Severity == SeverityFix || f.Severity == SeverityNit {
			out = append(out, f)
		}
	}
	return out
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
