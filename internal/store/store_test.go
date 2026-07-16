package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrateIdempotent(t *testing.T) {
	db := open(t)
	// A second migrate pass must be a no-op (all recorded).
	if err := db.migrate(context.Background()); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
}

func TestBookCRUDAndDedup(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	nb := NewBook{
		SourcePath: "/books/a", WorkDir: "/work/a", Title: "A Title",
		Authors: []string{"Author One"}, Series: "S", SeriesPos: "1",
		ASIN: "B01", IdentitySources: map[string]string{"asin": "tag"},
	}
	b, err := db.CreateBook(ctx, nb)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if b.ID == 0 || b.State != "queued" || b.Status != "" {
		t.Fatalf("unexpected new book: %+v", b)
	}
	if len(b.Authors) != 1 || b.Authors[0] != "Author One" {
		t.Fatalf("authors round-trip: %+v", b.Authors)
	}
	if b.IdentitySources["asin"] != "tag" {
		t.Fatalf("identity_sources round-trip: %+v", b.IdentitySources)
	}

	// Dedup on source_path.
	if _, err := db.CreateBook(ctx, nb); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate source_path: want ErrDuplicate, got %v", err)
	}

	got, err := db.GetBook(ctx, b.ID)
	if err != nil || got.Title != "A Title" {
		t.Fatalf("GetBook: %+v %v", got, err)
	}
	byPath, err := db.GetBookBySourcePath(ctx, "/books/a")
	if err != nil || byPath.ID != b.ID {
		t.Fatalf("GetBookBySourcePath: %+v %v", byPath, err)
	}
	if _, err := db.GetBook(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing book: want ErrNotFound, got %v", err)
	}

	// State + status + coverage mutations.
	if err := db.SetBookState(ctx, b.ID, "inspecting", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookStatus(ctx, b.ID, "paused", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookCoverage(ctx, b.ID, json.RawMessage(`{"known":true}`)); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetBook(ctx, b.ID)
	if got.State != "inspecting" || got.Status != "paused" || string(got.Coverage) != `{"known":true}` {
		t.Fatalf("after mutations: %+v", got)
	}

	list, err := db.ListBooks(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListBooks: %d %v", len(list), err)
	}

	if err := db.DeleteBook(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetBook(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
}

func TestStatusCheckConstraint(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/x", WorkDir: "/w", Title: "X"})
	if err := db.SetBookStatus(ctx, b.ID, "bogus", ""); err == nil {
		t.Fatal("status CHECK constraint should reject 'bogus'")
	}
}

func TestStageRunsAndReconcileHelpers(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/x", WorkDir: "/w", Title: "X"})

	runID, err := db.StartStageRun(ctx, b.ID, "asr", 1)
	if err != nil {
		t.Fatal(err)
	}
	// Open runs surface for reconcile.
	open, _ := db.OpenStageRuns(ctx)
	if len(open) != 1 || open[0].ID != runID || open[0].Ok != nil {
		t.Fatalf("OpenStageRuns: %+v", open)
	}
	if err := db.FinishStageRun(ctx, runID, true, json.RawMessage(`{"sec":3}`)); err != nil {
		t.Fatal(err)
	}
	open, _ = db.OpenStageRuns(ctx)
	if len(open) != 0 {
		t.Fatalf("finished run still open: %+v", open)
	}

	n, _ := db.CountStageRuns(ctx, b.ID, "asr")
	if n != 1 {
		t.Fatalf("CountStageRuns = %d", n)
	}
	ok, _ := db.CountStageSuccesses(ctx, b.ID, "asr")
	if ok != 1 {
		t.Fatalf("CountStageSuccesses = %d", ok)
	}
	succ, _ := db.SucceededStages(ctx, b.ID)
	if !succ["asr"] {
		t.Fatalf("SucceededStages = %+v", succ)
	}
	if err := db.DeleteStageSuccess(ctx, b.ID, "asr"); err != nil {
		t.Fatal(err)
	}
	succ, _ = db.SucceededStages(ctx, b.ID)
	if succ["asr"] {
		t.Fatalf("DeleteStageSuccess did not remove: %+v", succ)
	}

	runs, _ := db.ListStageRuns(ctx, b.ID)
	if len(runs) != 0 {
		t.Fatalf("ListStageRuns after delete = %d", len(runs))
	}
}

func TestProgress(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/x", WorkDir: "/w", Title: "X"})
	if err := db.SetProgress(ctx, b.ID, "asr", 2, 10); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProgress(ctx, b.ID, "asr", 5, 10); err != nil {
		t.Fatal(err)
	}
	p, ok, _ := db.GetProgress(ctx, b.ID, "asr")
	if !ok || p.Done != 5 || p.Total != 10 {
		t.Fatalf("GetProgress: %+v ok=%v", p, ok)
	}
	if _, ok, _ := db.GetProgress(ctx, b.ID, "missing"); ok {
		t.Fatal("missing progress reported present")
	}
	all, _ := db.ListProgress(ctx, b.ID)
	if len(all) != 1 {
		t.Fatalf("ListProgress = %d", len(all))
	}
}

func TestEventsLogAndPrune(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// One recent, one old.
	if err := db.InsertEvent(ctx, now, "book.state", 7, json.RawMessage(`{"state":"asr"}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertEvent(ctx, now.Add(-40*24*time.Hour), "queue.stats", 0, nil); err != nil {
		t.Fatal(err)
	}
	evs, _ := db.ListEvents(ctx, 0, 10)
	if len(evs) != 2 {
		t.Fatalf("ListEvents all = %d", len(evs))
	}
	byBook, _ := db.ListEvents(ctx, 7, 10)
	if len(byBook) != 1 || byBook[0].BookID == nil || *byBook[0].BookID != 7 {
		t.Fatalf("ListEvents by book = %+v", byBook)
	}
	removed, err := db.PruneEvents(ctx, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("PruneEvents removed = %d, want 1", removed)
	}
	evs, _ = db.ListEvents(ctx, 0, 10)
	if len(evs) != 1 {
		t.Fatalf("after prune = %d", len(evs))
	}
}

func TestSettingsAndRates(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if _, ok, _ := db.GetSetting(ctx, "k"); ok {
		t.Fatal("unset setting present")
	}
	if err := db.SetSetting(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(ctx, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	v, ok, _ := db.GetSetting(ctx, "k")
	if !ok || v != "v2" {
		t.Fatalf("GetSetting = %q ok=%v", v, ok)
	}
	if err := db.SetRate(ctx, "asr", 1.5); err != nil {
		t.Fatal(err)
	}
	r, ok, _ := db.GetRate(ctx, "asr")
	if !ok || r != 1.5 {
		t.Fatalf("GetRate = %v ok=%v", r, ok)
	}
}

func TestAuthStoreRoundTrip(t *testing.T) {
	db := open(t)
	as := db.AuthStore()
	// Password.
	if h, _ := as.LoadAuth(); h != "" {
		t.Fatalf("fresh LoadAuth = %q", h)
	}
	if err := as.SaveAuth("hash-1"); err != nil {
		t.Fatal(err)
	}
	if h, _ := as.LoadAuth(); h != "hash-1" {
		t.Fatalf("LoadAuth = %q", h)
	}
	// Sessions.
	if err := as.AddSession("tok-hash", time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := as.HasSession("tok-hash"); !ok {
		t.Fatal("session not found")
	}
	if ok, _ := as.HasSession("other"); ok {
		t.Fatal("unknown session reported present")
	}
	// Idempotent add.
	if err := as.AddSession("tok-hash", time.Now()); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if err := as.RemoveSession("tok-hash"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := as.HasSession("tok-hash"); ok {
		t.Fatal("session survived removal")
	}
}
