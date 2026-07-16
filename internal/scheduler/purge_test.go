package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// seedChapters creates a chapters/ dir with a dummy FLAC inside a book's work dir.
func seedChapters(t *testing.T, workDir string) string {
	t.Helper()
	dir := filepath.Join(workDir, "chapters")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ch001.flac"), []byte("flacdata"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	return dir
}

func TestPurgeScratchAllowedStates(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, h.workRoot)
	ctx := context.Background()

	// A done book: purge removes chapters/ and re-accounts scratch_bytes to what
	// remains (a durable manifest here).
	b := h.addBook(t, db, "done-book", "", "")
	chapters := seedChapters(t, b.WorkDir)
	if err := os.WriteFile(filepath.Join(b.WorkDir, "manifest.json"), []byte("durable"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if err := db.UpdateScratchBytes(ctx, b.ID, 999999); err != nil { // pre-purge accounting
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, b.ID, string(state.Done), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeScratch(ctx, b.ID); err != nil {
		t.Fatalf("PurgeScratch(done): %v", err)
	}
	if _, err := os.Stat(chapters); !os.IsNotExist(err) {
		t.Error("PurgeScratch(done) did not remove chapters/")
	}
	// scratch_bytes now reflects only the surviving durable (7 bytes), not the
	// pre-purge value.
	if got, _ := db.GetBook(ctx, b.ID); got.ScratchBytes != 7 {
		t.Errorf("PurgeScratch did not re-account scratch_bytes: got %d, want 7", got.ScratchBytes)
	}

	// A paused book is also purgeable.
	p := h.addBook(t, db, "paused-book", "", "")
	pch := seedChapters(t, p.WorkDir)
	if err := db.SetBookStatus(ctx, p.ID, string(state.StatusPaused), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeScratch(ctx, p.ID); err != nil {
		t.Fatalf("PurgeScratch(paused): %v", err)
	}
	if _, err := os.Stat(pch); !os.IsNotExist(err) {
		t.Error("PurgeScratch(paused) did not remove chapters/")
	}
}

func TestPurgeScratchRefusesRunning(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, h.workRoot)
	ctx := context.Background()

	// A running book (status none, non-terminal) must not be purgeable.
	b := h.addBook(t, db, "running-book", "", "")
	chapters := seedChapters(t, b.WorkDir)
	if err := db.SetBookState(ctx, b.ID, string(state.ASR), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeScratch(ctx, b.ID); err != ErrInvalidOp {
		t.Fatalf("PurgeScratch(running) err = %v, want ErrInvalidOp", err)
	}
	if _, err := os.Stat(chapters); err != nil {
		t.Error("PurgeScratch(running) removed chapters it must keep")
	}

	// Not found maps through.
	if err := s.PurgeScratch(ctx, 9999); err != store.ErrNotFound {
		t.Errorf("PurgeScratch(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPurgeReservationBusy proves the reservation primitive: while an id is
// reserved (as PurgeScratch holds it for its duration), a second reserve fails,
// a purge sees it busy, and Delete returns ErrBusy - so nothing races the removal.
func TestPurgeReservationBusy(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, h.workRoot)
	ctx := context.Background()

	p := h.addBook(t, db, "reserved", "", "")
	seedChapters(t, p.WorkDir)
	if err := db.SetBookStatus(ctx, p.ID, string(state.StatusPaused), ""); err != nil {
		t.Fatal(err)
	}
	if !s.reserve(p.ID) {
		t.Fatal("reserve on a free id should succeed")
	}
	if s.reserve(p.ID) {
		t.Error("reserve on an already-reserved id should fail")
	}
	if err := s.PurgeScratch(ctx, p.ID); err != ErrBusy {
		t.Errorf("PurgeScratch on a reserved id = %v, want ErrBusy", err)
	}
	if err := s.Delete(ctx, p.ID); err != ErrBusy {
		t.Errorf("Delete on a reserved id = %v, want ErrBusy", err)
	}
	s.unreserve(p.ID)
	// Freed: the purge now runs.
	if err := s.PurgeScratch(ctx, p.ID); err != nil {
		t.Errorf("PurgeScratch after unreserve = %v, want nil", err)
	}
}

// signalExecutor signals the first time it runs `signalStage`, then behaves like
// the stub (writes the sentinel, happy path). It lets a test observe exactly when
// a book's stage starts executing.
type signalExecutor struct {
	signalStage state.State
	started     chan struct{}
	once        sync.Once
}

func (e *signalExecutor) Execute(ctx context.Context, b store.Book, stage state.State, report ProgressFunc) (StageResult, error) {
	if stage == e.signalStage {
		e.once.Do(func() { close(e.started) })
	}
	res := happyPath()
	if err := WriteSentinel(b.WorkDir, string(stage), res); err != nil {
		return StageResult{}, err
	}
	return res, nil
}

// TestReservationBlocksDispatchUntilReleased is the item-2 interleave guard: a
// reservation held over an id (the window a purge occupies) prevents the running
// scheduler from starting that book - a Resume that lands during the reservation
// does not dispatch the book until the reservation is released.
func TestReservationBlocksDispatchUntilReleased(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "reserved-dispatch", "", "")
	// Park it paused at a runnable mechanical stage with chapters present.
	seedChapters(t, b.WorkDir)
	if err := db.SetBookState(ctx, b.ID, string(state.Splitting), string(state.StatusPaused), ""); err != nil {
		t.Fatal(err)
	}

	exec := &signalExecutor{signalStage: state.Splitting, started: make(chan struct{})}
	s := New(db, h.hub, exec, 2, h.workRoot)

	// Reserve BEFORE the loop runs (mirrors PurgeScratch holding the slot), so the
	// first dispatch pass cannot start the book even once we resume it.
	if !s.reserve(b.ID) {
		t.Fatal("reserve failed")
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = s.Start(runCtx); close(done) }()

	// Resume lands while the reservation is held: the book becomes dispatchable but
	// must NOT start until we release the reservation.
	if err := s.Resume(ctx, b.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	select {
	case <-exec.started:
		t.Fatal("book started while the reservation was held")
	case <-time.After(150 * time.Millisecond):
	}
	// Delete is refused for the same reason.
	if err := s.Delete(ctx, b.ID); err != ErrBusy {
		t.Errorf("Delete while reserved = %v, want ErrBusy", err)
	}

	// Release: the book now dispatches and its stage executes.
	s.unreserve(b.ID)
	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("book did not start after the reservation was released")
	}
	cancel()
	<-done
}

// splitReExecExecutor recreates a chapters/ marker and counts each splitting run,
// delegating every other stage to the stub - so a test can prove splitting really
// re-executed (rather than being skipped by the crash-resume fast path).
type splitReExecExecutor struct {
	stub   *StubExecutor
	mu     sync.Mutex
	splits int
}

func (e *splitReExecExecutor) Execute(ctx context.Context, b store.Book, stage state.State, report ProgressFunc) (StageResult, error) {
	if stage != state.Splitting {
		return e.stub.Execute(ctx, b, stage, report)
	}
	e.mu.Lock()
	e.splits++
	e.mu.Unlock()
	dir := filepath.Join(b.WorkDir, "chapters")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return StageResult{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "ch001.flac"), []byte("flacdata"), 0o644); err != nil { //nolint:gosec // test artifact
		return StageResult{}, err
	}
	res := happyPath()
	if err := WriteSentinel(b.WorkDir, string(stage), res); err != nil {
		return StageResult{}, err
	}
	return res, nil
}

// TestPurgeInvalidatesSplitSentinel is the item-5 regression: after a purge drops
// the split sentinel, resuming re-executes splitting (the crash-resume fast path
// must NOT skip it and leave chapters/ empty). Without the sentinel removal,
// runStage would see the sentinel and advance without re-splitting.
func TestPurgeInvalidatesSplitSentinel(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "resplit", "", "")

	// The crash-resume window shape: the book sits at splitting, paused, WITH a
	// splitting sentinel and its chapters on disk.
	chapters := seedChapters(t, b.WorkDir)
	if err := WriteSentinel(b.WorkDir, string(state.Splitting), happyPath()); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, b.ID, string(state.Splitting), string(state.StatusPaused), ""); err != nil {
		t.Fatal(err)
	}

	exec := &splitReExecExecutor{stub: NewStubExecutor(time.Millisecond, 2*time.Millisecond)}
	s := New(db, h.hub, exec, 2, h.workRoot)

	// Purge drops chapters/ AND the split sentinel.
	if err := s.PurgeScratch(ctx, b.ID); err != nil {
		t.Fatalf("PurgeScratch: %v", err)
	}
	if _, err := os.Stat(chapters); !os.IsNotExist(err) {
		t.Fatal("purge did not remove chapters/")
	}
	if SentinelExists(b.WorkDir, string(state.Splitting)) {
		t.Fatal("purge did not remove the split sentinel")
	}

	// Resume + run: splitting must re-execute (sentinel gone) and recreate chapters.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = s.Start(runCtx); close(done) }()
	if err := s.Resume(ctx, b.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	books := waitBooks(t, db, allDone, 15*time.Second)
	cancel()
	<-done
	if !allDone(books) {
		t.Fatal("book did not complete after purge+resume")
	}
	exec.mu.Lock()
	splits := exec.splits
	exec.mu.Unlock()
	if splits < 1 {
		t.Errorf("splitting re-executed %d times, want >= 1 (sentinel removal must force a re-split)", splits)
	}
	if _, err := os.Stat(chapters); err != nil {
		t.Errorf("chapters/ was not recreated by the re-split: %v", err)
	}
}

// waitBooks polls until pred holds over the book list or the deadline passes.
func waitBooks(t *testing.T, db *store.DB, pred func([]store.Book) bool, timeout time.Duration) []store.Book {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var books []store.Book
	for time.Now().Before(deadline) {
		books, _ = db.ListBooks(context.Background())
		if pred(books) {
			return books
		}
		time.Sleep(5 * time.Millisecond)
	}
	return books
}
