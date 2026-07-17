package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/canonical"
	"github.com/kodestar/audiosilo-meta/pkg/extract"
	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// ngramShingle is the shingle width for the no-verbatim overlap check (8-word runs);
// it matches the historical metaextract ngram default and the synthesis prompt's
// promise ("an 8-word-shingle check").
const ngramShingle = 8

// validateSidecarsStage is the MECHANICAL gate before the adversarial audit. It
// canonicalizes the sidecars in place (metafmt-equivalent), runs the shared
// structural validator, and runs the audiosilo-meta n-gram no-verbatim check against
// both transcript layers. Results are split by severity into validation_report.json's
// errors/warnings: a parse failure, a structural contract breach, or a verbatim
// overlap is an ERROR (which gates the effective audit pass); the advisory
// missing-book-2-recap is a WARNING (context for the auditor, never a blocker).
// Neither errors nor warnings park or fail the stage (they ride into auditing) - only
// an IO error fails it.
func (e *Executor) validateSidecarsStage(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if err := ctx.Err(); err != nil {
		return scheduler.StageResult{}, err
	}
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	start := time.Now()
	manifest, seriesOpener, _, err := e.sidecarStageInputs(ctx, book)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("validating: %w", err)
	}

	charsPath := filepath.Join(book.WorkDir, sidecarsDir, charactersFileName)
	recapsPath := filepath.Join(book.WorkDir, sidecarsDir, recapsFileName)
	if !fsutil.IsFile(charsPath) || !fsutil.IsFile(recapsPath) {
		return scheduler.StageResult{}, fmt.Errorf("validating: sidecars missing (synthesizing must run first)")
	}

	errs := []string{}
	warns := []string{}

	// Canonicalize in place; a parse failure is an ERROR finding (the auditor/fixer
	// must repair it), not an IO failure.
	for _, p := range []string{charsPath, recapsPath} {
		if f, err := canonicalizeInPlace(p); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("validating: canonicalize %s: %w", filepath.Base(p), err)
		} else if f != "" {
			errs = append(errs, f)
		}
	}

	// Structural checks (shared with synthesis/fix): its errors are ERRORS, its
	// advisory findings are WARNINGS. A decode failure is an ERROR.
	chars, recs, decodeErrs := decodeForValidation(charsPath, recapsPath)
	errs = append(errs, decodeErrs...)
	if chars != nil && recs != nil {
		structErrs, structWarns := validateSidecars(chars, recs, manifest.ChapterCount, seriesOpener)
		errs = append(errs, structErrs...)
		warns = append(warns, structWarns...)
	}

	// No-verbatim n-gram check against both transcript layers: every overlap is an ERROR.
	ngramFindings, err := ngramCheck(book.WorkDir, charsPath, recapsPath)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("validating: ngram check: %w", err)
	}
	errs = append(errs, ngramFindings...)

	if err := writeValidationReport(book.WorkDir, errs, warns); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("validating: write report: %w", err)
	}
	valSeconds := time.Since(start).Seconds()
	if r.Progress != nil {
		r.Progress(1, 1)
	}
	result := scheduler.StageResult{
		Metrics:    metrics(map[string]any{"errors": len(errs), "warnings": len(warns)}),
		RateSample: rateSample(1, valSeconds),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Validating), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// canonicalizeInPlace rewrites path to its canonical JSON form. It returns a finding
// (not an error) when the file is not valid JSON, so the audit/fix loop can repair a
// malformed sidecar; an IO read/write failure is returned as an error.
func canonicalizeInPlace(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return "", err
	}
	formatted, ferr := canonical.Format(raw)
	if ferr != nil {
		return fmt.Sprintf("%s is not valid JSON: %v", filepath.Base(path), ferr), nil
	}
	if err := fsutil.WriteFileAtomic(path, formatted, 0o644); err != nil {
		return "", err
	}
	return "", nil
}

// decodeForValidation strictly decodes both sidecars, turning a decode failure into
// a finding and returning nil for the file that failed so the structural checks skip
// it.
func decodeForValidation(charsPath, recapsPath string) (*model.Characters, *model.Recaps, []string) {
	var findings []string
	var chars *model.Characters
	var c model.Characters
	if err := decodeSidecarFile(charsPath, &c); err != nil {
		findings = append(findings, fmt.Sprintf("%s: %v", charactersFileName, err))
	} else {
		chars = &c
	}
	var recs *model.Recaps
	var r model.Recaps
	if err := decodeSidecarFile(recapsPath, &r); err != nil {
		findings = append(findings, fmt.Sprintf("%s: %v", recapsFileName, err))
	} else {
		recs = &r
	}
	return chars, recs, findings
}

// ngramCheck runs the audiosilo-meta shingle-overlap check over the sidecars against
// both the transcripts-text/ and transcripts-corrected/ layers, per source file. Each
// overlap is a finding naming the locus, the source layer, and the offending run. A
// layer that does not exist (or holds no .txt files) is skipped; a genuine read
// failure inside the check is returned as an error.
func ngramCheck(workDir, charsPath, recapsPath string) ([]string, error) {
	var findings []string
	sidecars := []string{charsPath, recapsPath}
	sources := []struct {
		label string
		dir   string
	}{
		{"transcripts-text", filepath.Join(workDir, transcript.TextDir)},
		{"transcripts-corrected", filepath.Join(workDir, spelling.CorrectedDir)},
	}
	for _, src := range sources {
		if !hasTxtFiles(src.dir) {
			continue
		}
		hits, err := extract.NGram(src.dir, sidecars, ngramShingle)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src.label, err)
		}
		for _, h := range hits {
			findings = append(findings, fmt.Sprintf(
				"near-verbatim overlap (%d words) at %s in %s vs %s: %q",
				h.Words, h.Locus, filepath.Base(h.File), src.label, h.Text))
		}
	}
	return findings, nil
}

// hasTxtFiles reports whether dir exists and contains at least one .txt file (the
// only inputs extract.NGram reads from a directory source).
func hasTxtFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, ent := range entries {
		if !ent.IsDir() && filepath.Ext(ent.Name()) == ".txt" {
			return true
		}
	}
	return false
}

// writeValidationReport writes validation_report.json {clean, errors, warnings}. Each
// list is never nil (an empty slice renders as [] rather than null); clean is
// len(errors)==0, so a warning-only report is clean.
func writeValidationReport(workDir string, errs, warns []string) error {
	if errs == nil {
		errs = []string{}
	}
	if warns == nil {
		warns = []string{}
	}
	rep := validationReport{Clean: len(errs) == 0, Errors: errs, Warnings: warns}
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, validationReportName), append(out, '\n'), 0o644)
}

// loadValidationReport reads validation_report.json (validating's output) for the
// audit stage's clean-flag consistency check.
func loadValidationReport(workDir string) (validationReport, error) {
	var rep validationReport
	raw, err := os.ReadFile(filepath.Join(workDir, validationReportName)) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return rep, err
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}
