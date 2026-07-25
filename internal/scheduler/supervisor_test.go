package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

func TestSupervisorTerminateRequeueClosesOrphanedRun(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "orphan", "", "")
	if err := db.SetBookState(context.Background(), b.ID, string(state.FactPass), "", "", ""); err != nil {
		t.Fatal(err)
	}
	runID, err := db.StartStageRun(context.Background(), b.ID, string(state.FactPass), 1)
	if err != nil {
		t.Fatal(err)
	}
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)
	outcome, err := s.SupervisorApply(context.Background(), "terminate_requeue", b.ID, string(state.FactPass))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "orphaned database run closed and requeued" {
		t.Fatalf("outcome=%q", outcome)
	}
	runs, err := db.ListStageRuns(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != runID || runs[0].Ok == nil || *runs[0].Ok {
		t.Fatalf("orphan run was not durably failed: %+v", runs)
	}
}

func TestSupervisorSupersedeRerunPreservesSpendAndRemovesLaterSentinels(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "rerun", "", "")
	if err := db.SetBookState(context.Background(), b.ID, string(state.Synthesizing), "", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []state.State{state.FactPass, state.Synthesizing} {
		runID, err := db.StartStageRun(context.Background(), b.ID, string(stage), 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AddOpenStageRunUsage(context.Background(), b.ID, string(stage), "model", 100, 10, 0.25); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishStageRun(context.Background(), runID, true, json.RawMessage(`{"ok":true}`)); err != nil {
			t.Fatal(err)
		}
		if err := WriteSentinel(b.WorkDir, string(stage), StageResult{}); err != nil {
			t.Fatal(err)
		}
	}
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)
	if _, err := s.SupervisorApply(context.Background(), "supersede_rerun", b.ID, string(state.FactPass)); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetBook(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(state.FactPass) {
		t.Fatalf("state=%q", got.State)
	}
	for _, stage := range []state.State{state.FactPass, state.Synthesizing} {
		if _, err := os.Stat(SentinelPath(b.WorkDir, string(stage))); !os.IsNotExist(err) {
			t.Fatalf("%s sentinel still exists: %v", stage, err)
		}
	}
	runs, err := db.ListStageRuns(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || !runs[0].Superseded || !runs[1].Superseded {
		t.Fatalf("runs not superseded: %+v", runs)
	}
	if cost, err := db.SumStageRunCost(context.Background(), b.ID); err != nil || cost != 0.5 {
		t.Fatalf("preserved cost=%v err=%v", cost, err)
	}
}

func TestSupervisorReadmitRetainsExistingRateLimitWindow(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "rate", "", "")
	due := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := db.SetBookStatusRetry(context.Background(), b.ID, string(state.StatusNeedsAttention), "rate limited", string(state.ParkAgentRateLimited), due); err != nil {
		t.Fatal(err)
	}
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)
	outcome, err := s.SupervisorApply(context.Background(), "readmit", b.ID, b.State)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "existing transient readmission window retained until "+due {
		t.Fatalf("outcome=%q", outcome)
	}
	got, err := db.GetBook(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != string(state.StatusNeedsAttention) || got.RetryAt != due {
		t.Fatalf("rate-limit window changed: %+v", got)
	}
}

func TestSupervisorParkIncludesIncidentDetail(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "detailed-park", "", "")
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)
	const detail = "authentication failed: agent CLI is not logged in"
	if _, err := s.SupervisorApply(context.Background(), "park_escalate", b.ID, b.State, detail); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetBook(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error != "supervisor: "+detail || got.ParkCode != string(state.ParkSupervisorEscalated) {
		t.Fatalf("park did not retain incident detail: %+v", got)
	}
}

// TestSupervisorParkEscalatePreservesUnderlyingParkCode: escalating a book that is ALREADY
// parked with a typed code must update only the operator-facing message and keep the
// underlying park code, so its Retry/readmit semantics survive. Rewriting a wrapped
// fix_loop_exhausted park to supervisor_escalated made a manual Retry take the plain readmit
// branch (no auditing/fixing supersede, no trajectory wipe), so the audit stage re-entered
// with an exhausted round history and re-parked instantly. It also confirms an escalation of
// an UNPARKED (running) book still stamps supervisor_escalated.
func TestSupervisorParkEscalatePreservesUnderlyingParkCode(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)

	// A book already parked fix_loop_exhausted at auditing, with its fix budget spent, one
	// auditing success, and the audit-loop trajectory artifacts on disk.
	b := h.addBook(t, db, "wrapped-park", "", "")
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

	const detail = "operator flagged the stuck audit loop"
	if _, err := s.SupervisorApply(ctx, "park_escalate", b.ID, b.State, detail); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetBook(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The escalation updated only the message; the underlying park code is preserved.
	if got.ParkCode != string(state.ParkFixLoopExhausted) {
		t.Fatalf("park_code=%q after escalation, want fix_loop_exhausted preserved", got.ParkCode)
	}
	if got.Error != state.SupervisorMessagePrefix+detail {
		t.Fatalf("error=%q, want the escalation message", got.Error)
	}

	// A Retry now takes the fix_loop_exhausted readmit branch: auditing AND fixing
	// successes are superseded and the trajectory files wiped, granting a genuinely fresh
	// loop instead of an instant re-park.
	if err := s.Retry(ctx, b.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n, _ := db.CountStageSuccesses(ctx, b.ID, string(state.Auditing)); n != 0 {
		t.Errorf("auditing successes = %d after retry, want 0 (fresh loop)", n)
	}
	if n, _ := db.CountStageSuccesses(ctx, b.ID, string(state.Fixing)); n != 0 {
		t.Errorf("fixing successes = %d after retry, want 0 (fresh loop)", n)
	}
	for _, name := range []string{AuditRoundsFile, AuditAcceptedFile} {
		if _, err := os.Stat(filepath.Join(b.WorkDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still present after retry (stat err %v), want removed", name, err)
		}
	}

	// An UNPARKED (running) book escalated for the first time still stamps supervisor_escalated.
	run := h.addBook(t, db, "running-escalate", "", "")
	if err := db.SetBookState(ctx, run.ID, string(state.Auditing), "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SupervisorApply(ctx, "park_escalate", run.ID, run.State, "generic escalation"); err != nil {
		t.Fatal(err)
	}
	gotRun, err := db.GetBook(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != string(state.StatusNeedsAttention) || gotRun.ParkCode != string(state.ParkSupervisorEscalated) {
		t.Fatalf("unparked escalation: status=%q park_code=%q, want needs_attention/supervisor_escalated", gotRun.Status, gotRun.ParkCode)
	}
}

func TestSupervisorNeverRewindsReadyOrPublishedOutput(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "published", "", "")
	if err := db.SetBookState(context.Background(), b.ID, string(state.Done), "", "", ""); err != nil {
		t.Fatal(err)
	}
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)
	if _, err := s.SupervisorApply(context.Background(), "supersede_rerun", b.ID, string(state.Validating)); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("published rewind err=%v, want ErrInvalidOp", err)
	}
	got, err := db.GetBook(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(state.Done) {
		t.Fatalf("published book rewound: %+v", got)
	}
}

func TestSupervisorRerunPreservesDeliberatePause(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "paused-rerun", "", "")
	if err := db.SetBookState(context.Background(), b.ID, string(state.Validating), string(state.StatusPaused), "paused by operator", ""); err != nil {
		t.Fatal(err)
	}
	runID, err := db.StartStageRun(context.Background(), b.ID, string(state.QASweep), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(context.Background(), runID, true, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)
	if _, err := s.SupervisorApply(context.Background(), "supersede_rerun", b.ID, string(state.QASweep)); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetBook(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(state.QASweep) || got.Status != string(state.StatusPaused) || got.Error != "paused by operator" {
		t.Fatalf("paused rerun changed operator status: %+v", got)
	}
}
