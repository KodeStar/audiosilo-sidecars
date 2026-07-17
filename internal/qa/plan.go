package qa

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
)

// PlanFile is the qa_adjudicating stage's output artifact: the agent's disposition of
// every actionable QA finding.
const PlanFile = "qa_plan.json"

// PlanAction is how one flagged chapter is dispositioned. "retranscribe" re-runs ASR
// on the whole chapter; "tail_clip" cuts and re-transcribes only the loop window and
// splices it; "accept" leaves the chapter as-is (a benign finding).
type PlanAction string

const (
	ActionRetranscribe PlanAction = "retranscribe"
	ActionTailClip     PlanAction = "tail_clip"
	ActionAccept       PlanAction = "accept"
)

// PlanEntry is one chapter's disposition with the agent's justification.
type PlanEntry struct {
	Chapter int        `json:"chapter"`
	Action  PlanAction `json:"action"`
	Reason  string     `json:"reason"`
}

// Plan is the qa_adjudicating agent's output: one entry per actionable finding plus
// optional free-text notes.
type Plan struct {
	Entries []PlanEntry `json:"entries"`
	Notes   string      `json:"notes,omitempty"`
}

// RetranscribeNeeded reports whether any entry asks for work beyond acceptance - the
// signal the pipeline maps onto StageResult.RetranscribeNeeded (qa_adjudicating ->
// retranscribing when true, else -> spelling_research).
func (p *Plan) RetranscribeNeeded() bool {
	for _, e := range p.Entries {
		if e.Action != ActionAccept {
			return true
		}
	}
	return false
}

// LoadReport reads qa_report.json from workDir (the qa_adjudicating stage's
// precondition artifact). It is the counterpart to WriteReport.
func LoadReport(workDir string) (*Report, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, ReportJSONName)) //nolint:gosec // path derives from the book's own work dir
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ReportJSONName, err)
	}
	return &r, nil
}

// WritePlan writes p to workDir/qa_plan.json (pretty JSON, trailing newline). The
// qa_adjudicating stage writes the merged auto-accept + agent plan directly rather
// than harvesting the raw agent file, so the auto-accepted entries the daemon
// pre-filled survive into the persisted plan.
func WritePlan(workDir string, p *Plan) error {
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, PlanFile), append(out, '\n'), 0o644)
}

// LoadPlan reads qa_plan.json from workDir.
func LoadPlan(workDir string) (*Plan, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, PlanFile)) //nolint:gosec // path derives from the book's own work dir
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", PlanFile, err)
	}
	return &p, nil
}

// Validate checks the plan against the QA report: every chapter that REQUIRES a
// disposition (the retranscribe queue, every tail-rate hit, and every mid-chapter
// repeated-run / multi-loop) has exactly one entry; no entry names a chapter the sweep
// did not flag at all; every action is valid and every reason non-empty. The
// informational low-confidence stats and benign end-fade runs never require an entry
// but a chapter they touch may still legitimately carry an "accept".
func (p *Plan) Validate(rep *Report) error {
	if rep == nil {
		return errors.New("qa plan: nil report")
	}
	required := requiredChapters(rep)
	allowed := allowedChapters(rep)

	seen := make(map[int]int, len(p.Entries))
	for _, e := range p.Entries {
		switch e.Action {
		case ActionRetranscribe, ActionTailClip, ActionAccept:
		default:
			return fmt.Errorf("qa plan: chapter %d has invalid action %q", e.Chapter, e.Action)
		}
		if strings.TrimSpace(e.Reason) == "" {
			return fmt.Errorf("qa plan: chapter %d has an empty reason", e.Chapter)
		}
		if !allowed[e.Chapter] {
			return fmt.Errorf("qa plan: chapter %d has an entry but the QA sweep flagged nothing for it", e.Chapter)
		}
		seen[e.Chapter]++
	}
	// Deterministic error ordering: report the lowest offending chapter first.
	dups := make([]int, 0)
	for ch, n := range seen {
		if n > 1 {
			dups = append(dups, ch)
		}
	}
	sort.Ints(dups)
	if len(dups) > 0 {
		return fmt.Errorf("qa plan: chapter %d has %d entries (expected exactly one)", dups[0], seen[dups[0]])
	}
	missing := make([]int, 0)
	for ch := range required {
		if seen[ch] == 0 {
			missing = append(missing, ch)
		}
	}
	sort.Ints(missing)
	if len(missing) > 0 {
		return fmt.Errorf("qa plan: chapter %d is flagged for disposition but has no plan entry", missing[0])
	}
	return nil
}

// FlaggedChapters is the sorted set of chapters that REQUIRE a disposition - the same
// set (*Plan).Validate forces a plan entry for: the retranscribe queue, every tail-rate
// hit, and every MID-CHAPTER repeated-run and multi-loop. The qa_adjudicating stage
// stages exactly these chapters' transcripts for the agent, so staging and validation
// share one definition of "flagged" (no drift between what the agent sees and what its
// plan must cover).
func FlaggedChapters(rep *Report) []int {
	set := requiredChapters(rep)
	out := make([]int, 0, len(set))
	for ch := range set {
		out = append(out, ch)
	}
	sort.Ints(out)
	return out
}

// requiredChapters is the set that MUST be dispositioned: the retranscribe queue, every
// tail-rate hit, and every MID-CHAPTER repeated-run and multi-loop (the dangerous
// findings that overwrite real narration). End fades and cross/within-segment hits are
// not forced (they ride into spelling/fact-pass adjudication).
func requiredChapters(rep *Report) map[int]bool {
	set := make(map[int]bool)
	for _, ch := range rep.RetranscribeQueue {
		set[ch] = true
	}
	for _, h := range rep.TailRate {
		set[h.Chapter] = true
	}
	for _, r := range rep.RepeatedRuns {
		if r.Kind == KindMidChapter {
			set[r.Chapter] = true
		}
	}
	for _, f := range rep.MultiLoop {
		if f.MidChapter {
			set[f.Chapter] = true
		}
	}
	return set
}

// allowedChapters is every chapter carrying ANY substantive finding (the superset of
// requiredChapters): a plan entry outside this set names a chapter the sweep did not
// flag, which is rejected. Low-confidence stats are informational and excluded.
func allowedChapters(rep *Report) map[int]bool {
	set := requiredChapters(rep)
	for _, o := range rep.WPHOutliers {
		set[o.Chapter] = true
	}
	for _, r := range rep.RepeatedRuns {
		set[r.Chapter] = true
	}
	for _, h := range rep.CrossSegment {
		set[h.Chapter] = true
	}
	for _, h := range rep.WithinSegment {
		set[h.Chapter] = true
	}
	for _, f := range rep.MultiLoop {
		set[f.Chapter] = true
	}
	return set
}
