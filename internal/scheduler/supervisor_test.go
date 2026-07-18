package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
