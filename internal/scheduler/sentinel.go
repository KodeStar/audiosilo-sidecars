package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// StageResult is what a stage execution reports back: the branch decisions the
// state machine consults (only the fields relevant to the stage matter) plus
// opaque metrics. It is persisted inside the sentinel so a crash-resume that
// skips re-execution can still recover the branch and advance correctly.
type StageResult struct {
	MarkersContiguous  bool            `json:"markers_contiguous,omitempty"`
	QAClean            bool            `json:"qa_clean,omitempty"`
	RetranscribeNeeded bool            `json:"retranscribe_needed,omitempty"`
	AuditPassed        bool            `json:"audit_passed,omitempty"`
	Metrics            json.RawMessage `json:"metrics,omitempty"`
	// RateSample is the stage's own report of how much work it did this run (see
	// RateSample). nil = no rate observation, which the scheduler treats as "don't
	// update the learned rate". It persists in the sentinel (additive, omitempty) but
	// is never re-folded on a crash-resume skip, only on a genuine execution.
	RateSample *RateSample `json:"rate_sample,omitempty"`
	// ParkMessage is an optional richer reason a stage attaches for a park the
	// SCHEDULER decides (not the stage). The auditing stage fills it with the
	// fix-count trajectory so advance()'s fix-loop-exhausted park can surface why the
	// book did not converge, instead of the generic hardcoded message. Empty = use the
	// scheduler's default. Additive/omitempty; persisted so a crash-resume park keeps it.
	ParkMessage string `json:"park_message,omitempty"`
}

// Audit-loop trajectory artifacts, written by the pipeline's auditing stage in a
// book's work dir. The scheduler owns their NAMES - like the _done sentinels - so
// Retry can wipe them for a fresh fix loop without importing the pipeline package
// (which would invert the dependency direction). The pipeline owns their JSON schema.
const (
	AuditRoundsFile   = "audit_rounds.json"
	AuditAcceptedFile = "audit_accepted.json"
)

// Sentinel is the on-disk _done/<stage>.json marker: the CONTENT truth that a
// stage produced its output. Runs counts real executions (so a test can prove a
// completed stage was never re-run after a crash), and Result carries the branch
// decision the scheduler needs to advance even when it skips re-execution.
type Sentinel struct {
	Stage  string      `json:"stage"`
	Runs   int         `json:"runs"`
	At     string      `json:"at"`
	Result StageResult `json:"result"`
}

// doneDir is the sentinel directory inside a book's work dir.
func doneDir(workDir string) string { return filepath.Join(workDir, "_done") }

// SentinelPath returns the sentinel file path for a stage.
func SentinelPath(workDir, stage string) string {
	return filepath.Join(doneDir(workDir), stage+".json")
}

// SentinelExists reports whether a stage's sentinel is present.
func SentinelExists(workDir, stage string) bool {
	_, err := os.Stat(SentinelPath(workDir, stage))
	return err == nil
}

// ReadSentinel loads a stage's sentinel.
func ReadSentinel(workDir, stage string) (Sentinel, error) {
	var s Sentinel
	raw, err := os.ReadFile(SentinelPath(workDir, stage)) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return Sentinel{}, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Sentinel{}, err
	}
	return s, nil
}

// WriteSentinel records a completed stage, incrementing the run counter from any
// existing sentinel. Executors call this as their final durable action, so the
// scheduler's "skip if the sentinel exists" check is safe: a crash after this
// write means the stage is genuinely done and must not re-run. The write is
// atomic (temp file + rename).
func WriteSentinel(workDir, stage string, result StageResult) error {
	if err := os.MkdirAll(doneDir(workDir), 0o750); err != nil {
		return err
	}
	runs := 0
	if prev, err := ReadSentinel(workDir, stage); err == nil {
		runs = prev.Runs
	}
	s := Sentinel{Stage: stage, Runs: runs + 1, At: time.Now().UTC().Format(time.RFC3339Nano), Result: result}
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := SentinelPath(workDir, stage)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil { //nolint:gosec // sentinel is non-secret
		return err
	}
	return os.Rename(tmp, path)
}
