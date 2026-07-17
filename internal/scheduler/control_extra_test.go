package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// hookExecutor writes the happy-path sentinel like the stub, but runs a hook
// AFTER the sentinel is written and BEFORE it returns - so a test can inject an
// action (e.g. a pause) into the exact window between "stage done on disk" and
// "scheduler advances the state".
type hookExecutor struct {
	after func(b store.Book, stage state.State)
}

func (e *hookExecutor) Execute(_ context.Context, b store.Book, stage state.State, _ ProgressFunc) (StageResult, error) {
	res := happyPath()
	if err := WriteSentinel(b.WorkDir, string(stage), res); err != nil {
		return StageResult{}, err
	}
	if e.after != nil {
		e.after(b, stage)
	}
	return res, nil
}

// noSentinelExecutor returns success but never writes the stage sentinel - a
// stage-implementation bug the scheduler must catch loudly rather than spin on.
type noSentinelExecutor struct{}

func (noSentinelExecutor) Execute(_ context.Context, _ store.Book, _ state.State, _ ProgressFunc) (StageResult, error) {
	return happyPath(), nil
}

// TestStageSuccessWithoutSentinelFails is the item-8 regression: a stage that returns
// success without writing its sentinel is turned into a terminal failure (with a
// descriptive message) instead of looping forever through reconcile's rewind.
func TestStageSuccessWithoutSentinelFails(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "nosentinel", "", "")

	books := runUntil(t, db, h.hub, noSentinelExecutor{}, 1, func(bs []store.Book) bool {
		for _, bk := range bs {
			if bk.Status == string(state.StatusFailed) {
				return true
			}
		}
		return false
	}, 5*time.Second)

	var got store.Book
	for _, bk := range books {
		if bk.ID == b.ID {
			got = bk
		}
	}
	if got.Status != string(state.StatusFailed) {
		t.Fatalf("book status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "without writing its sentinel") {
		t.Errorf("book error = %q, want the sentinel-bug message", got.Error)
	}
}

// TestPauseAfterSentinelBeforeAdvanceStaysPaused is the F1 regression: a pause
// that lands in the window between a stage's sentinel write and the scheduler's
// state advance must NOT be clobbered - advance moves the pipeline state only,
// never status/error.
func TestPauseAfterSentinelBeforeAdvanceStaysPaused(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "pausewin", "", "")

	var once sync.Once
	exec := &hookExecutor{after: func(bk store.Book, stage state.State) {
		if stage == state.Inspecting {
			once.Do(func() {
				_ = db.SetBookStatus(context.Background(), bk.ID, string(state.StatusPaused), "")
			})
		}
	}}
	sched := New(db, h.hub, exec, 1, h.workRoot)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = sched.Start(ctx); close(done) }()
	sched.Notify()

	// It advances past inspecting once (to splitting) then the pause holds it.
	deadline := time.Now().Add(3 * time.Second)
	var cur store.Book
	for time.Now().Before(deadline) {
		cur, _ = db.GetBook(context.Background(), b.ID)
		if cur.State == string(state.Splitting) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if cur.State != string(state.Splitting) {
		t.Fatalf("book did not advance to splitting: state=%s status=%s", cur.State, cur.Status)
	}
	if cur.Status != string(state.StatusPaused) {
		t.Fatalf("advance clobbered the pause: status=%q, want paused", cur.Status)
	}
	// It stays put: a paused book is not dispatched further.
	time.Sleep(120 * time.Millisecond)
	cur, _ = db.GetBook(context.Background(), b.ID)
	if state.Order(state.State(cur.State)) > state.Order(state.Splitting) {
		t.Fatalf("paused book advanced past splitting: %s", cur.State)
	}
	cancel()
	<-done
}

// TestBookStateEventCarriesError is the F8 regression: book.state events include
// the book's error string, so a client can surface a failure/cancel reason.
func TestBookStateEventCarriesError(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "errbook", "", "")
	sched := New(db, h.hub, NewStubExecutor(0, 0), 2, h.workRoot)

	_, sub := h.hub.Subscribe(0)
	defer sub.Close()

	if err := sched.Cancel(context.Background(), b.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-sub.C:
			if ev.Type != "book.state" {
				continue
			}
			var p struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				t.Fatalf("decode event: %v", err)
			}
			if p.Status == string(state.StatusFailed) {
				if p.Error != "cancelled by user" {
					t.Fatalf("book.state error = %q, want 'cancelled by user'", p.Error)
				}
				return
			}
		case <-deadline:
			t.Fatal("no book.state event carrying the cancel error was observed")
		}
	}
}

// TestQALoopReExecutesUntilClean is the F2b regression: a loop-back into an
// already-executed stage must re-execute it (advance clears the stale sentinel),
// not replay the frozen outcome - otherwise qa_sweep would replay "unclean"
// forever and the pipeline would never terminate.
func TestQALoopReExecutesUntilClean(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	h.addBook(t, db, "loop", "", "")

	exec := NewStubExecutor(1*time.Millisecond, 3*time.Millisecond)
	var mu sync.Mutex
	qaRuns, adjRuns := 0, 0
	exec.Decide = func(_ store.Book, stage state.State) StageResult {
		mu.Lock()
		defer mu.Unlock()
		r := StageResult{MarkersContiguous: true, AuditPassed: true}
		switch stage {
		case state.QASweep:
			qaRuns++
			r.QAClean = qaRuns >= 2 // first pass unclean -> adjudicate -> retranscribe -> clean
		case state.QAAdjudicating:
			adjRuns++
			r.RetranscribeNeeded = true
		}
		return r
	}

	books := runUntil(t, db, h.hub, exec, 2, allDone, 15*time.Second)
	if !allDone(books) {
		t.Fatal("QA loop did not terminate (a frozen sentinel would loop forever)")
	}
	b := books[0]

	mu.Lock()
	gotQA, gotAdj := qaRuns, adjRuns
	mu.Unlock()
	if gotQA != 2 {
		t.Errorf("qa_sweep executed %d times, want 2 (unclean then clean)", gotQA)
	}
	if gotAdj != 1 {
		t.Errorf("qa_adjudicating executed %d times, want 1", gotAdj)
	}

	// The final sentinel is freshly rewritten (Runs=1), proving the stale one was
	// cleared on re-entry rather than skipped.
	sn, err := ReadSentinel(b.WorkDir, string(state.QASweep))
	if err != nil {
		t.Fatalf("qa_sweep sentinel: %v", err)
	}
	if sn.Runs != 1 {
		t.Errorf("qa_sweep sentinel Runs = %d, want 1 (rewritten fresh on re-entry)", sn.Runs)
	}

	// Two ok stage_run rows for qa_sweep (one per real execution).
	runs, _ := db.ListStageRuns(context.Background(), b.ID)
	total, ok := 0, 0
	for _, r := range runs {
		if r.Stage == string(state.QASweep) {
			total++
			if r.Ok != nil && *r.Ok {
				ok++
			}
		}
	}
	if total != 2 || ok != 2 {
		t.Errorf("qa_sweep stage_runs total=%d ok=%d, want 2/2", total, ok)
	}
}

// TestCrashWindowResumeNoExtraRun is the F2a regression: on resume, a stage whose
// sentinel exists but whose run was interrupted (crash between sentinel write and
// advance) must be skipped via advance WITHOUT opening a new stage_run - so it
// never records a duplicate ok run.
func TestCrashWindowResumeNoExtraRun(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "crashwin", "", "")

	// Simulate the crash window: the book sits at 'inspecting', that stage's
	// sentinel is on disk, but its run is still OPEN (never finished/advanced).
	_ = db.SetBookState(ctx, b.ID, string(state.Inspecting), "", "")
	_, _ = db.StartStageRun(ctx, b.ID, string(state.Inspecting), 1)
	_ = WriteSentinel(b.WorkDir, string(state.Inspecting), StageResult{MarkersContiguous: true})

	// Resume: reconcile closes the open run failed; dispatch skips re-execution and
	// advances. Then the stub carries the book to done.
	exec := NewStubExecutor(1*time.Millisecond, 3*time.Millisecond)
	books := runUntil(t, db, h.hub, exec, 2, allDone, 15*time.Second)
	if !allDone(books) {
		t.Fatal("did not resume to completion")
	}

	// inspecting has exactly ONE run: the reconcile-closed (failed) one. The skip
	// path must NOT have added an ok=1 run.
	runs, _ := db.ListStageRuns(ctx, b.ID)
	total, ok := 0, 0
	for _, r := range runs {
		if r.Stage == string(state.Inspecting) {
			total++
			if r.Ok != nil && *r.Ok {
				ok++
			}
		}
	}
	if total != 1 || ok != 0 {
		t.Fatalf("inspecting stage_runs total=%d ok=%d, want 1/0 (no extra ok on skip-resume)", total, ok)
	}
}

// TestDeleteRemovesWorkDirWithinRoot is the F14 regression: Delete removes a
// book's work dir when it lives inside the work root, and NEVER touches a dir
// outside it.
func TestDeleteRemovesWorkDirWithinRoot(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	sched := New(db, h.hub, NewStubExecutor(0, 0), 2, h.workRoot)

	// A book whose work dir is under the work root: delete removes it.
	b := h.addBook(t, db, "delme", "", "")
	if err := os.MkdirAll(filepath.Join(b.WorkDir, "_done"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := sched.Delete(ctx, b.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(b.WorkDir); !os.IsNotExist(err) {
		t.Fatalf("work dir survived delete: err=%v", err)
	}

	// A book whose work dir is OUTSIDE the work root is never removed by delete.
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.MkdirAll(keep, 0o750); err != nil {
		t.Fatal(err)
	}
	b2, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/o", WorkDir: outside, Title: "O"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sched.Delete(ctx, b2.ID); err != nil {
		t.Fatalf("delete outside: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("delete wrongly removed a dir outside the work root: %v", err)
	}
}
