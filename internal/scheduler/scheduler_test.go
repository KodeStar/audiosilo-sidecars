package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// harness bundles a file-backed store (so it survives a scheduler restart), a
// hub, and a work-dir root.
type harness struct {
	dbPath   string
	workRoot string
	hub      *events.Hub
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	return &harness{
		dbPath:   filepath.Join(dir, "sidecars.db"),
		workRoot: filepath.Join(dir, "work"),
		hub:      events.NewHub(1024),
	}
}

func (h *harness) openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), h.dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (h *harness) addBook(t *testing.T, db *store.DB, slug, series, pos string) store.Book {
	t.Helper()
	b, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: "/src/" + slug,
		WorkDir:    filepath.Join(h.workRoot, slug),
		Title:      slug,
		Series:     series,
		SeriesPos:  pos,
	})
	if err != nil {
		t.Fatalf("create book %s: %v", slug, err)
	}
	return b
}

// runUntil runs a scheduler in the background and waits until pred(books) holds
// or the deadline passes. It returns the final books and cancels the scheduler.
func runUntil(t *testing.T, db *store.DB, hub *events.Hub, exec Executor, agentCap int,
	pred func([]store.Book) bool, timeout time.Duration) []store.Book {
	t.Helper()
	sched := New(db, hub, exec, agentCap, "", false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = sched.Start(ctx); close(done) }()
	sched.Notify()

	deadline := time.Now().Add(timeout)
	var books []store.Book
	for time.Now().Before(deadline) {
		books, _ = db.ListBooks(context.Background())
		if pred(books) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	return books
}

func allDone(books []store.Book) bool {
	for _, b := range books {
		if b.State != string(state.Done) {
			return false
		}
	}
	return len(books) > 0
}

// happy-path stages executed per book (markers_normalizing + qa_adjudicating
// skipped; no retranscribe/fixing).
var happyStages = []state.State{
	state.Inspecting, state.Splitting, state.ASR, state.Sanitizing, state.QASweep,
	state.SpellingResearch, state.Correcting, state.FactPass, state.Synthesizing,
	state.Validating, state.Auditing, state.Contributing,
}

func TestPipelineRunsToDone(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	h.addBook(t, db, "book-a", "Series One", "1")
	h.addBook(t, db, "book-b", "Series Two", "1")
	h.addBook(t, db, "book-c", "", "")

	exec := NewStubExecutor(2*time.Millisecond, 6*time.Millisecond)
	books := runUntil(t, db, h.hub, exec, 2, allDone, 15*time.Second)
	if !allDone(books) {
		for _, b := range books {
			t.Logf("book %d %s state=%s status=%s", b.ID, b.Title, b.State, b.Status)
		}
		t.Fatal("not all books reached done")
	}
	// Each completed stage ran exactly once.
	for _, b := range books {
		for _, st := range happyStages {
			sn, err := ReadSentinel(b.WorkDir, string(st))
			if err != nil {
				t.Errorf("%s: sentinel %s missing: %v", b.Title, st, err)
				continue
			}
			if sn.Runs != 1 {
				t.Errorf("%s stage %s ran %d times, want 1", b.Title, st, sn.Runs)
			}
		}
		// Skipped conditionals have no sentinel.
		if SentinelExists(b.WorkDir, string(state.MarkersNormalizing)) {
			t.Errorf("%s: markers_normalizing should have been skipped", b.Title)
		}
	}
}

func TestCrashResumeNoDuplicateStages(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	const n = 6
	for i := 0; i < n; i++ {
		h.addBook(t, db, string(rune('a'+i))+"-book", "", "")
	}

	// First run: cancel mid-flight (once at least one book has made progress but
	// not everything is done).
	exec := NewStubExecutor(4*time.Millisecond, 10*time.Millisecond)
	sched := New(db, h.hub, exec, 2, h.workRoot, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = sched.Start(ctx); close(done) }()
	sched.Notify()

	// Wait until some but not all books are past 'asr', then cut power.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		books, _ := db.ListBooks(context.Background())
		progressed, finished := 0, 0
		for _, b := range books {
			if state.Order(state.State(b.State)) > state.Order(state.ASR) {
				progressed++
			}
			if b.State == string(state.Done) {
				finished++
			}
		}
		if progressed >= 2 && finished < n {
			break
		}
		time.Sleep(3 * time.Millisecond)
	}
	cancel()
	<-done
	_ = db.Close()

	// Rebuild from the store (fresh DB handle + fresh scheduler) and resume.
	db2, err := store.Open(context.Background(), h.dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	exec2 := NewStubExecutor(2*time.Millisecond, 6*time.Millisecond)
	books := runUntil(t, db2, h.hub, exec2, 2, allDone, 20*time.Second)
	if !allDone(books) {
		t.Fatal("did not resume to completion")
	}
	// The invariant: no completed stage was executed twice across the restart.
	for _, b := range books {
		for _, st := range happyStages {
			sn, err := ReadSentinel(b.WorkDir, string(st))
			if err != nil {
				t.Errorf("%s: sentinel %s missing after resume: %v", b.Title, st, err)
				continue
			}
			if sn.Runs != 1 {
				t.Errorf("%s stage %s ran %d times across restart, want 1", b.Title, st, sn.Runs)
			}
		}
	}
}

func TestPauseResume(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "paused-book", "", "")

	// Pause before the scheduler starts: it must not progress.
	if err := New(db, h.hub, NewStubExecutor(0, 0), 2, h.workRoot, false).Pause(context.Background(), b.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	exec := NewStubExecutor(2*time.Millisecond, 5*time.Millisecond)
	notDone := func(books []store.Book) bool { return books[0].State == string(state.Done) }
	books := runUntil(t, db, h.hub, exec, 2, notDone, 400*time.Millisecond)
	if books[0].State == string(state.Done) {
		t.Fatal("paused book progressed to done")
	}
	if books[0].Status != string(state.StatusPaused) {
		t.Fatalf("status = %q, want paused", books[0].Status)
	}

	// Resume: it now runs to done.
	sched := New(db, h.hub, exec, 2, h.workRoot, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sched.Start(ctx) }()
	if err := sched.Resume(context.Background(), b.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := db.GetBook(context.Background(), b.ID)
		if cur.State == string(state.Done) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("resumed book did not reach done")
}
