package benchmark

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// CalibrationResult records fresh holdout judgments of an accepted production
// reference. It estimates judge strictness; it is not a generation benchmark and
// must be kept in a separate results root.
type CalibrationResult struct {
	Version     int             `json:"version"`
	Profile     string          `json:"judge_profile"`
	Case        string          `json:"case"`
	InputSHA256 string          `json:"input_sha256"`
	StartedAt   string          `json:"started_at"`
	FinishedAt  string          `json:"finished_at"`
	Holdouts    []HoldoutResult `json:"holdouts"`
}

// Calibrate judges the frozen accepted reference with the selected profile's
// holdouts. Requiring one explicit profile and case prevents accidentally paying
// for duplicate judges shared by every profile in a matrix.
func Calibrate(ctx context.Context, opts RunOptions) (result CalibrationResult, retErr error) {
	if strings.TrimSpace(opts.ResultsDir) == "" || opts.ProfileID == "" || opts.ProfileID == "all" || opts.CaseID == "" || opts.CaseID == "all" {
		return result, fmt.Errorf("calibrate requires a results directory and one explicit profile and case")
	}
	suite, err := LoadSuite(opts.SuitePath)
	if err != nil {
		return result, err
	}
	matrix, err := LoadMatrix(opts.MatrixPath)
	if err != nil {
		return result, err
	}
	profiles, cases := selectProfiles(matrix, opts.ProfileID), selectCases(suite, opts.CaseID)
	if len(profiles) != 1 || len(cases) != 1 {
		return result, fmt.Errorf("calibration profile or case not found")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Minute
	}
	p, c := profiles[0], cases[0]
	base := filepath.Dir(opts.SuitePath)
	input := filepath.Join(base, filepath.FromSlash(c.InputDir))
	digest, err := treeDigest(input)
	if err != nil {
		return result, err
	}
	if digest != c.SHA256 {
		return result, fmt.Errorf("case %q input digest changed: got %s, want %s", c.ID, digest, c.SHA256)
	}
	started := time.Now()
	result = CalibrationResult{Version: schemaVersion, Profile: p.ID, Case: c.ID, InputSHA256: c.SHA256, StartedAt: started.UTC().Format(time.RFC3339Nano)}
	runDir := filepath.Join(opts.ResultsDir, p.ID, c.ID)
	if err := ensureNewDir(runDir); err != nil {
		return result, err
	}
	defer func() {
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := writeJSON(filepath.Join(runDir, "calibration.json"), result); err != nil && retErr == nil {
			retErr = err
		}
	}()
	sourceWork := filepath.Join(runDir, "accepted-reference")
	if err := copySelected(input, sourceWork, []string{"manifest.json"}, nil, true); err != nil {
		return result, err
	}
	reference := filepath.Join(base, filepath.FromSlash(c.Reference))
	if err := copySelected(reference, sourceWork,
		[]string{"spellings.json", "validation_report.json"}, []string{"facts", "sidecars"}, true); err != nil {
		return result, err
	}
	for i, judge := range p.Judges {
		holdout, err := runHoldout(ctx, opts, matrix, c, p, judge, sourceWork, runDir, i)
		result.Holdouts = append(result.Holdouts, holdout)
		if err != nil {
			return result, fmt.Errorf("reference holdout judge %d: %w", i+1, err)
		}
	}
	return result, nil
}
