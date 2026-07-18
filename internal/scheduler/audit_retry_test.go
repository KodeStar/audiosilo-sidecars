package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

// TestRetryAuditingGrantsFreshFixLoop: a book parked at auditing with its fix budget
// spent gets a genuinely fresh loop on Retry - the fixing successes are dropped (so
// FixAttempts restarts at 0) and the audit-loop trajectory artifacts are wiped, rather
// than burning one audit and instantly re-parking.
func TestRetryAuditingGrantsFreshFixLoop(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "audretry", "", "")

	// Park it at auditing needs_attention with the fix budget spent.
	if err := db.SetBookState(ctx, b.ID, string(state.Auditing), string(state.StatusNeedsAttention),
		"audit did not converge after 3 fix rounds", string(state.ParkFixLoopExhausted)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < state.MaxFixAttempts; i++ {
		id, err := db.StartStageRun(ctx, b.ID, string(state.Fixing), i+1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishStageRun(ctx, id, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	aid, err := db.StartStageRun(ctx, b.ID, string(state.Auditing), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(ctx, aid, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b.WorkDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{AuditRoundsFile, AuditAcceptedFile} {
		if err := os.WriteFile(filepath.Join(b.WorkDir, name), []byte("[]"), 0o644); err != nil { //nolint:gosec // test artifact
			t.Fatal(err)
		}
	}

	sched := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	if err := sched.Retry(ctx, b.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}

	if n, _ := db.CountStageSuccesses(ctx, b.ID, string(state.Fixing)); n != 0 {
		t.Errorf("fixing successes = %d after retry, want 0 (fresh loop)", n)
	}
	for _, name := range []string{AuditRoundsFile, AuditAcceptedFile} {
		if _, err := os.Stat(filepath.Join(b.WorkDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still present after retry (stat err %v), want removed", name, err)
		}
	}
	cur, _ := db.GetBook(ctx, b.ID)
	if cur.Status != "" || cur.ParkCode != "" {
		t.Errorf("after retry status=%q park_code=%q, want both cleared", cur.Status, cur.ParkCode)
	}
}

// TestRetryAuditingKeepsFixHistoryForOtherParkCode: a book parked at auditing for a
// reason OTHER than an exhausted fix loop (here budget_exceeded) must NOT have its
// fixing successes superseded on Retry - the fresh-loop grant is keyed on the park
// code, not merely being at the auditing stage, so genuine fix progress survives.
func TestRetryAuditingKeepsFixHistoryForOtherParkCode(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "audbudget", "", "")

	// Park it at auditing needs_attention, but with a DIFFERENT park code (budget).
	if err := db.SetBookState(ctx, b.ID, string(state.Auditing), string(state.StatusNeedsAttention),
		"book agent cost reached the budget", string(state.ParkBudgetExceeded)); err != nil {
		t.Fatal(err)
	}
	// Record some genuine fixing successes that must be preserved.
	const fixRuns = 2
	for i := 0; i < fixRuns; i++ {
		id, err := db.StartStageRun(ctx, b.ID, string(state.Fixing), i+1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishStageRun(ctx, id, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	aid, err := db.StartStageRun(ctx, b.ID, string(state.Auditing), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(ctx, aid, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b.WorkDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{AuditRoundsFile, AuditAcceptedFile} {
		if err := os.WriteFile(filepath.Join(b.WorkDir, name), []byte("[]"), 0o644); err != nil { //nolint:gosec // test artifact
			t.Fatal(err)
		}
	}

	sched := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	if err := sched.Retry(ctx, b.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}

	// Fixing successes are preserved (the fresh-loop grant did not fire).
	if n, _ := db.CountStageSuccesses(ctx, b.ID, string(state.Fixing)); n != fixRuns {
		t.Errorf("fixing successes = %d after retry, want %d (history preserved)", n, fixRuns)
	}
	// The audit-loop trajectory artifacts are NOT wiped.
	for _, name := range []string{AuditRoundsFile, AuditAcceptedFile} {
		if _, err := os.Stat(filepath.Join(b.WorkDir, name)); err != nil {
			t.Errorf("%s missing after retry (stat err %v), want preserved", name, err)
		}
	}
	// The status/park_code still clear (a normal readmit).
	cur, _ := db.GetBook(ctx, b.ID)
	if cur.Status != "" || cur.ParkCode != "" {
		t.Errorf("after retry status=%q park_code=%q, want both cleared", cur.Status, cur.ParkCode)
	}
}

// TestReadmitAvailabilityParkKeepsCurrentStageSuccesses: a book parked on a TRANSIENT
// availability condition (agent_rate_limited) at auditing must keep its auditing success
// rows across readmit. Superseding the current stage is reserved for the ROUND-CAP parks
// (qa_no_converge / fix_loop_exhausted); an availability readmit that wiped them would reset
// the audit round count and destroy the convergence trajectory the book had already built
// (the audit stage's done==0 reset would then delete audit_rounds.json/audit_accepted.json).
func TestReadmitAvailabilityParkKeepsCurrentStageSuccesses(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "audratelimit", "", "")

	if err := db.SetBookState(ctx, b.ID, string(state.Auditing), string(state.StatusNeedsAttention),
		"agent backend is rate-limited", string(state.ParkAgentRateLimited)); err != nil {
		t.Fatal(err)
	}
	const auditRuns = 2
	for i := 0; i < auditRuns; i++ {
		id, err := db.StartStageRun(ctx, b.ID, string(state.Auditing), i+1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishStageRun(ctx, id, true, nil); err != nil {
			t.Fatal(err)
		}
	}

	sched := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	if err := sched.Retry(ctx, b.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}

	// The auditing successes survive (the availability readmit did not supersede them).
	if n, _ := db.CountStageSuccesses(ctx, b.ID, string(state.Auditing)); n != auditRuns {
		t.Errorf("auditing successes = %d after readmit, want %d (availability park keeps the trajectory)", n, auditRuns)
	}
	cur, _ := db.GetBook(ctx, b.ID)
	if cur.Status != "" || cur.ParkCode != "" {
		t.Errorf("after retry status=%q park_code=%q, want both cleared", cur.Status, cur.ParkCode)
	}
}

// TestReadmitRoundCapParkSupersedesCurrentStage is the positive branch: a book parked
// qa_no_converge at qa_adjudicating gets its QA round counter reset on readmit (the
// "grant one fresh round" contract), so its qa_adjudicating successes ARE superseded
// (CountStageSuccesses -> 0) while the availability parks above leave theirs intact.
func TestReadmitRoundCapParkSupersedesCurrentStage(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "qacap", "", "")

	if err := db.SetBookState(ctx, b.ID, string(state.QAAdjudicating), string(state.StatusNeedsAttention),
		"QA adjudication did not converge", string(state.ParkQANoConverge)); err != nil {
		t.Fatal(err)
	}
	const rounds = 3
	for i := 0; i < rounds; i++ {
		id, err := db.StartStageRun(ctx, b.ID, string(state.QAAdjudicating), i+1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishStageRun(ctx, id, true, nil); err != nil {
			t.Fatal(err)
		}
	}

	sched := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	if err := sched.Retry(ctx, b.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n, _ := db.CountStageSuccesses(ctx, b.ID, string(state.QAAdjudicating)); n != 0 {
		t.Errorf("qa_adjudicating successes = %d after readmit, want 0 (round-cap park grants a fresh round)", n)
	}
}
