package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
)

// auditAcceptMaxFix is the largest FIX count the auditing stage will ACCEPT-AND-FINISH
// on rather than keep looping. A large book's adversarial audit samples ~1 genuinely
// new small defect per pass, so the fix==0 pass bar is unreachable; once the fix
// trajectory is small and non-growing, one or two residual FIX items are cheaper to
// apply-and-ship than to keep paying ~$6 per audit+fix round chasing zero. Calibrated
// to the real book-3 evidence (fix trajectory 4 -> 1 -> 1 -> 2, blocker 0), which
// converged and should have accepted at round 2 instead of parking.
const auditAcceptMaxFix = 2

// auditRound is one entry in audit_rounds.json: the per-round finding tally the
// acceptance decision and the park trajectory message read. Appended once per broad
// adversarial audit round, never for the later targeted verification.
type auditRound struct {
	Round   int `json:"round"`
	Blocker int `json:"blocker"`
	Fix     int `json:"fix"`
	Nit     int `json:"nit"`
}

// auditAccepted is the audit_accepted.json marker written when a round accepts-and-
// finishes: the round it accepted on, the residual FIX/NIT counts, and this round's
// FIX+NIT findings (so the record shows exactly what was accepted). Its presence on the
// next auditing entry (after the final fix re-validates clean) drives a focused semantic
// verification; it is also the durable "we converged" record the contribution note reads.
type auditAccepted struct {
	Round    int            `json:"round"`
	Fix      int            `json:"fix"`
	Nit      int            `json:"nit"`
	Findings []AuditFinding `json:"findings"`
}

// auditRoundsPath / auditAcceptedPath are the trajectory artifacts' work-dir paths. The
// scheduler owns the file NAMES (scheduler.AuditRoundsFile / AuditAcceptedFile) so Retry
// can wipe them without importing this package; the schema lives here.
func auditRoundsPath(workDir string) string {
	return filepath.Join(workDir, scheduler.AuditRoundsFile)
}

func auditAcceptedPath(workDir string) string {
	return filepath.Join(workDir, scheduler.AuditAcceptedFile)
}

// loadAuditRounds reads the per-round history, tolerating an absent or unreadable file
// (returns nil - a fresh history). A malformed file is treated as empty rather than
// failing the stage: the history is advisory (it drives acceptance and the park
// message), never a correctness gate.
func loadAuditRounds(workDir string) []auditRound {
	raw, err := os.ReadFile(auditRoundsPath(workDir)) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return nil
	}
	var rounds []auditRound
	if err := json.Unmarshal(raw, &rounds); err != nil {
		return nil
	}
	return rounds
}

// appendAuditRound appends one round to the history and writes it back atomically.
func appendAuditRound(workDir string, round auditRound) error {
	rounds := append(loadAuditRounds(workDir), round)
	out, err := json.MarshalIndent(rounds, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(auditRoundsPath(workDir), append(out, '\n'), 0o644)
}

// loadAuditAccepted reads the acceptance marker, returning ok=false when it is absent or
// unreadable/malformed (no acceptance in flight).
func loadAuditAccepted(workDir string) (auditAccepted, bool) {
	raw, err := os.ReadFile(auditAcceptedPath(workDir)) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return auditAccepted{}, false
	}
	var acc auditAccepted
	if err := json.Unmarshal(raw, &acc); err != nil {
		return auditAccepted{}, false
	}
	return acc, true
}

// writeAuditAccepted writes the acceptance marker atomically.
func writeAuditAccepted(workDir string, acc auditAccepted) error {
	out, err := json.MarshalIndent(acc, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(auditAcceptedPath(workDir), append(out, '\n'), 0o644)
}

// removeAuditTrajectory drops both trajectory artifacts. Called on the done==0 stage
// entry (a fresh admit / Retry / purge-rewind) so a new fix loop never inherits a prior
// life's history or a stale acceptance marker. Best-effort (a missing file is fine).
func removeAuditTrajectory(workDir string) {
	_ = os.Remove(auditRoundsPath(workDir))
	_ = os.Remove(auditAcceptedPath(workDir))
}

// acceptTrajectory decides whether a non-passing audit round should ACCEPT-AND-FINISH
// (apply this round's FIX items in one last fixing pass, then pass on re-entry) instead
// of continuing the loop toward the park. Every clause must hold:
//   - blocker == 0 and validation is clean: a BLOCKER or a mechanically-fixable
//     validation error is never accepted over (those are real, fixable defects).
//   - round >= 2: at least one full fix round already ran and was re-audited, so we are
//     judging a trajectory, not a first look.
//   - 0 < fix <= auditAcceptMaxFix: fix==0 is a normal pass (handled elsewhere); more
//     than the cap is too much residual to auto-ship.
//   - prevOK and fix <= prevBlocker+prevFix: the actionable count is not growing.
//     A BLOCKER becoming a FIX is progress, not the misleading "fix 0 -> 1"
//     regression that used to reject a one-item tail.
//   - fixesDone < MaxFixAttempts: the budget still allows the ONE final fixing round the
//     acceptance path needs to apply this round's items.
func acceptTrajectory(round, blocker, fix, prevBlocker, prevFix int, prevOK bool, fixesDone int, valClean bool, maxFix int) bool {
	if blocker != 0 || !valClean {
		return false
	}
	if round < 2 {
		return false
	}
	if fix <= 0 || fix > auditAcceptMaxFix {
		return false
	}
	if !prevOK || fix > prevBlocker+prevFix {
		return false
	}
	return fixesDone < maxFix
}

// fixTrajectory renders the history's FIX counts as "4 -> 1 -> 2" for a message.
func fixTrajectory(rounds []auditRound) string {
	nums := make([]string, 0, len(rounds))
	for _, r := range rounds {
		nums = append(nums, strconv.Itoa(r.Fix))
	}
	return strings.Join(nums, " -> ")
}

// fixLoopParkMessage builds the fix-loop-exhausted park reason from the full history
// (including the terminal round), surfacing the fix-count trajectory and any residual
// blockers so the park explains why the book did not converge. fixesDone is the number
// of fixing rounds spent.
func fixLoopParkMessage(rounds []auditRound, fixesDone int) string {
	if len(rounds) == 0 {
		return "audit did not converge before the fix-loop budget was spent"
	}
	msg := fmt.Sprintf("audit did not converge after %d fix round(s) (fix counts %s", fixesDone, fixTrajectory(rounds))
	if last := rounds[len(rounds)-1]; last.Blocker > 0 {
		msg += fmt.Sprintf(", blockers %d", last.Blocker)
	}
	return msg + ")"
}

// auditAcceptanceNote renders the contribution-row note appended when the sidecars were
// accepted on a converging trajectory (residual nits recorded rather than chased to
// zero). Empty when no acceptance marker exists (a normal clean pass).
func auditAcceptanceNote(workDir string) string {
	acc, ok := loadAuditAccepted(workDir)
	if !ok {
		return ""
	}
	return fmt.Sprintf("audit converged after %d rounds; %d residual nit(s) recorded in %s",
		acc.Round, acc.Nit, scheduler.AuditAcceptedFile)
}
