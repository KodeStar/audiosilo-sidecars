package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

func bk(id int64, series, pos, st string) store.Book {
	return store.Book{ID: id, Series: series, SeriesPos: pos, State: st}
}

func TestLockHoldersSeriesOrdering(t *testing.T) {
	// Same series, positions 1 and 2, both mid-pipeline: only pos 1 holds.
	books := []store.Book{
		bk(1, "S", "1", string(state.FactPass)),
		bk(2, "S", "2", string(state.ASR)),
	}
	h := lockHolders(books)
	if !h[1] || h[2] {
		t.Fatalf("holders = %v, want only book 1", h)
	}

	// Pos 1 reaches ready: pos 2 becomes the holder.
	books[0].State = string(state.Ready)
	h = lockHolders(books)
	if h[1] || !h[2] {
		t.Fatalf("after pos1 ready, holders = %v, want only book 2", h)
	}
}

func TestLockHoldersParkedPredecessorHoldsSeries(t *testing.T) {
	// Pos 1 is parked (needs_attention) mid-pipeline; it is still unfinished, so
	// it retains the series lock and pos 2 stays blocked from agent work until
	// pos 1 is resumed or cancelled.
	books := []store.Book{
		bk(1, "S", "1", string(state.Auditing)),
		bk(2, "S", "2", string(state.ASR)),
	}
	books[0].Status = string(state.StatusNeedsAttention)
	h := lockHolders(books)
	if !h[1] || h[2] {
		t.Fatalf("parked predecessor: holders = %v, want only book 1", h)
	}
}

func TestLockHoldersSerieslessAndCrossSeriesParallelize(t *testing.T) {
	books := []store.Book{
		bk(1, "", "", string(state.FactPass)),
		bk(2, "", "", string(state.Synthesizing)),
		bk(3, "A", "1", string(state.FactPass)),
		bk(4, "B", "1", string(state.FactPass)),
	}
	h := lockHolders(books)
	for _, id := range []int64{1, 2, 3, 4} {
		if !h[id] {
			t.Errorf("book %d should hold (seriesless or distinct series): %v", id, h)
		}
	}
}

func TestParkedSeriesDoesNotDeadlockOtherSeries(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	// Series X pos 1 will park at the audit (fix budget spent); pos 2 waits on it.
	h.addBook(t, db, "x1", "X", "1")
	h.addBook(t, db, "x2", "X", "2")
	// An independent seriesless book must still finish.
	free := h.addBook(t, db, "free", "", "")

	exec := NewStubExecutor(2*time.Millisecond, 5*time.Millisecond)
	// x1 always fails the audit -> parks needs_attention after the fix cap.
	exec.Decide = func(b store.Book, stage state.State) StageResult {
		r := StageResult{MarkersContiguous: true, QAClean: true}
		if b.Title == "x1" && stage == state.Auditing {
			r.AuditPassed = false
		} else {
			r.AuditPassed = true
		}
		return r
	}

	pred := func(books []store.Book) bool {
		var x1Parked, freeDone bool
		for _, b := range books {
			if b.ID == free.ID && b.State == string(state.Done) {
				freeDone = true
			}
			if b.Title == "x1" && b.Status == string(state.StatusNeedsAttention) {
				x1Parked = true
			}
		}
		return x1Parked && freeDone
	}
	books := runUntil(t, db, h.hub, exec, 2, pred, 20*time.Second)

	var x1, x2, freeb store.Book
	for _, b := range books {
		switch b.Title {
		case "x1":
			x1 = b
		case "x2":
			x2 = b
		case "free":
			freeb = b
		}
	}
	if x1.Status != string(state.StatusNeedsAttention) {
		t.Fatalf("x1 status = %q, want needs_attention", x1.Status)
	}
	if freeb.State != string(state.Done) {
		t.Fatalf("independent book blocked: state=%s", freeb.State)
	}
	// x2 is held before agent work by the parked predecessor: it may have done
	// mechanical/asr work but must NOT have passed its (agent) fact_pass.
	if state.Order(state.State(x2.State)) > state.Order(state.FactPass) {
		t.Fatalf("x2 advanced past agent gate despite parked predecessor: %s", x2.State)
	}
}

func TestReconcileClosesOpenRunsAndRewindsOnMissingSentinel(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "recon", "", "")

	// Simulate a crash: an open (never-finished) run, a completed 'inspecting'
	// with its sentinel, and a completed 'asr' whose sentinel is MISSING.
	_ = db.SetBookState(ctx, b.ID, string(state.Sanitizing), "", "", "")
	openID, _ := db.StartStageRun(ctx, b.ID, string(state.Sanitizing), 1) // left open
	insID, _ := db.StartStageRun(ctx, b.ID, string(state.Inspecting), 1)
	_ = db.FinishStageRun(ctx, insID, true, nil)
	_ = WriteSentinel(b.WorkDir, string(state.Inspecting), StageResult{})
	asrID, _ := db.StartStageRun(ctx, b.ID, string(state.ASR), 1)
	_ = db.FinishStageRun(ctx, asrID, true, nil)
	// NOTE: no ASR sentinel written -> the reconcile must rewind to asr.

	sched := New(db, h.hub, NewStubExecutor(0, 0), 2, h.workRoot)
	sched.ctx = ctx
	if err := sched.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The open run is now closed failed.
	if open, _ := db.OpenStageRuns(ctx); len(open) != 0 {
		t.Fatalf("open runs after reconcile = %d, want 0", len(open))
	}
	runs, _ := db.ListStageRuns(ctx, b.ID)
	var interrupted *store.StageRun
	for i := range runs {
		if runs[i].ID == openID {
			interrupted = &runs[i]
		}
	}
	if interrupted == nil || interrupted.FinishedAt == "" || interrupted.Ok == nil || *interrupted.Ok {
		t.Fatalf("interrupted run not closed failed: %+v", interrupted)
	}
	// The book rewound to asr (earliest completed stage missing its sentinel).
	cur, _ := db.GetBook(ctx, b.ID)
	if cur.State != string(state.ASR) {
		t.Fatalf("rewound to %q, want asr", cur.State)
	}
	// asr's success was dropped; inspecting's kept.
	allSucc, _ := db.SucceededStagesAll(ctx)
	succ := allSucc[b.ID]
	if succ[string(state.ASR)] {
		t.Error("asr success should have been dropped")
	}
	if !succ[string(state.Inspecting)] {
		t.Error("inspecting success should be preserved")
	}
}

func TestControlOpErrors(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "ctrl", "", "")
	sched := New(db, h.hub, NewStubExecutor(0, 0), 2, h.workRoot)

	// Resume a non-paused book -> invalid.
	if err := sched.Resume(ctx, b.ID); err != ErrInvalidOp {
		t.Errorf("resume non-paused = %v, want ErrInvalidOp", err)
	}
	// Retry a non-failed book -> invalid.
	if err := sched.Retry(ctx, b.ID); err != ErrInvalidOp {
		t.Errorf("retry non-failed = %v, want ErrInvalidOp", err)
	}
	// Missing book.
	if err := sched.Pause(ctx, 9999); err != store.ErrNotFound {
		t.Errorf("pause missing = %v, want ErrNotFound", err)
	}
	// Cancel then retry works.
	if err := sched.Cancel(ctx, b.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	cur, _ := db.GetBook(ctx, b.ID)
	if cur.Status != string(state.StatusFailed) {
		t.Fatalf("cancelled status = %q", cur.Status)
	}
	if err := sched.Retry(ctx, b.ID); err != nil {
		t.Fatalf("retry after cancel: %v", err)
	}
	// Delete works when not in-flight.
	if err := sched.Delete(ctx, b.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
