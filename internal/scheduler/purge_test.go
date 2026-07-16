package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

	// A done book: purge removes chapters/.
	b := h.addBook(t, db, "done-book", "", "")
	chapters := seedChapters(t, b.WorkDir)
	if err := db.SetBookState(ctx, b.ID, string(state.Done), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeScratch(ctx, b.ID); err != nil {
		t.Fatalf("PurgeScratch(done): %v", err)
	}
	if _, err := os.Stat(chapters); !os.IsNotExist(err) {
		t.Error("PurgeScratch(done) did not remove chapters/")
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
