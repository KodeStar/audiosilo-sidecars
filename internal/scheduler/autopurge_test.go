package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

// TestAutoPurgeOnReachingDone drives advance(Contributing -> Done) with the knob on and
// asserts the scratch is reclaimed (chapters gone, scratch_bytes re-accounted, split
// sentinel dropped).
func TestAutoPurgeOnReachingDone(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, h.workRoot, true)
	s.ctx = context.Background()
	ctx := context.Background()

	b := h.addBook(t, db, "done-book", "", "")
	chapters := seedChapters(t, b.WorkDir)
	if err := WriteSentinel(b.WorkDir, string(state.Splitting), StageResult{}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateScratchBytes(ctx, b.ID, 999999); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, b.ID, string(state.Contributing), "", "", ""); err != nil {
		t.Fatal(err)
	}
	b.State = string(state.Contributing)

	s.advance(ctx, b, state.Contributing, StageResult{})

	got, _ := db.GetBook(ctx, b.ID)
	if got.State != string(state.Done) {
		t.Fatalf("state = %q, want done", got.State)
	}
	if _, err := os.Stat(chapters); !os.IsNotExist(err) {
		t.Error("auto-purge did not remove chapters/")
	}
	if got.ScratchBytes >= 999999 {
		t.Errorf("scratch_bytes = %d, want re-accounted below the pre-purge value", got.ScratchBytes)
	}
	if SentinelExists(b.WorkDir, string(state.Splitting)) {
		t.Error("auto-purge did not drop the split sentinel")
	}
}

// TestAutoPurgeOffLeavesScratch proves the knob gates the reclaim.
func TestAutoPurgeOffLeavesScratch(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, h.workRoot, false)
	// autoPurge off.
	s.ctx = context.Background()
	ctx := context.Background()

	b := h.addBook(t, db, "keep-book", "", "")
	chapters := seedChapters(t, b.WorkDir)
	if err := db.SetBookState(ctx, b.ID, string(state.Contributing), "", "", ""); err != nil {
		t.Fatal(err)
	}
	b.State = string(state.Contributing)

	s.advance(ctx, b, state.Contributing, StageResult{})

	if got, _ := db.GetBook(ctx, b.ID); got.State != string(state.Done) {
		t.Fatalf("state = %q, want done", got.State)
	}
	if _, err := os.Stat(chapters); err != nil {
		t.Error("knob off must leave chapters/ intact")
	}
}

// TestStartupGCPurgesDoneBooks proves Reconcile reclaims scratch for already-done books
// when the knob is on, and leaves them when off.
func TestStartupGCPurgesDoneBooks(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()

	// A done book with chapters + accounted scratch.
	b := h.addBook(t, db, "old-done", "", "")
	chapters := seedChapters(t, b.WorkDir)
	if err := os.WriteFile(filepath.Join(b.WorkDir, "manifest.json"), []byte("durable"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if err := db.UpdateScratchBytes(ctx, b.ID, 999999); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, b.ID, string(state.Done), "", "", ""); err != nil {
		t.Fatal(err)
	}

	// Knob on: Reconcile starts the GC in a background goroutine (off the dispatch
	// path), so poll for the reclaim rather than assume it finished synchronously.
	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, h.workRoot, true)
	if err := s.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	waitUntil(t, func() bool {
		_, err := os.Stat(chapters)
		return os.IsNotExist(err)
	}, "startup GC did not remove chapters/ of a done book")
	if got, _ := db.GetBook(ctx, b.ID); got.ScratchBytes >= 999999 {
		t.Errorf("scratch_bytes = %d, want re-accounted", got.ScratchBytes)
	}
}

// TestStartupGCSkipsReservedBook proves the startup GC reserves each book before
// purging and skips one already reserved/busy (a concurrent PurgeScratch/Delete holds
// the slot), so it never races another operation on the same work dir.
func TestStartupGCSkipsReservedBook(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()

	b := h.addBook(t, db, "reserved-done", "", "")
	chapters := seedChapters(t, b.WorkDir)
	if err := db.UpdateScratchBytes(ctx, b.ID, 999999); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, b.ID, string(state.Done), "", "", ""); err != nil {
		t.Fatal(err)
	}

	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, h.workRoot, true)
	// Hold the slot, as a concurrent purge/delete would.
	if !s.reserve(b.ID) {
		t.Fatal("reserve failed")
	}
	defer s.unreserve(b.ID)

	books, err := db.ListBooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.startupGC(ctx, books)

	if _, err := os.Stat(chapters); err != nil {
		t.Errorf("startup GC purged a reserved book (did not skip it): %v", err)
	}
}

// waitUntil polls cond up to a short timeout, failing with msg if it never holds. It
// lets a test observe an effect produced by a background goroutine (the async startup
// GC) deterministically without a fixed sleep.
func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error(msg)
}

// TestPurgeAccountingSurvivesCancelledCtx proves the scratch re-accounting runs even
// under a cancelled context (a shutdown-timed auto-purge/startup-GC): the files are
// already deleted, so skipping the gauge write would leave scratch_bytes overstating
// disk that is gone.
func TestPurgeAccountingSurvivesCancelledCtx(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)

	b := h.addBook(t, db, "cancel-purge", "", "")
	seedChapters(t, b.WorkDir)
	if err := os.WriteFile(filepath.Join(b.WorkDir, "manifest.json"), []byte("durable"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if err := db.UpdateScratchBytes(context.Background(), b.ID, 999999); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(context.Background(), b.ID, string(state.Done), "", "", ""); err != nil {
		t.Fatal(err)
	}
	b.State = string(state.Done)

	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, h.workRoot, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already-cancelled, as at shutdown
	if err := s.purgeScratchInner(ctx, b); err != nil {
		t.Fatalf("purgeScratchInner: %v", err)
	}
	if got, _ := db.GetBook(context.Background(), b.ID); got.ScratchBytes >= 999999 {
		t.Errorf("scratch_bytes = %d, want re-accounted below the pre-purge value despite a cancelled ctx", got.ScratchBytes)
	}
}

// TestStartupGCKnobOff proves Reconcile leaves done-book scratch when the knob is off.
func TestStartupGCKnobOff(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()

	b := h.addBook(t, db, "old-done-keep", "", "")
	chapters := seedChapters(t, b.WorkDir)
	if err := db.UpdateScratchBytes(ctx, b.ID, 999999); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, b.ID, string(state.Done), "", "", ""); err != nil {
		t.Fatal(err)
	}

	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, h.workRoot, false)
	// autoPurge off.
	if err := s.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(chapters); err != nil {
		t.Error("knob off: startup GC must leave chapters/ intact")
	}
}
