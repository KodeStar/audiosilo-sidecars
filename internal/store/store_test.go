package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/auth"
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
		Coverage: json.RawMessage(`{"available":true,"known":true}`),
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
	if string(b.Coverage) != `{"available":true,"known":true}` {
		t.Fatalf("coverage round-trip: %s", b.Coverage)
	}

	// Dedup on source_path.
	if _, err := db.CreateBook(ctx, nb); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate source_path: want ErrDuplicate, got %v", err)
	}

	got, err := db.GetBook(ctx, b.ID)
	if err != nil || got.Title != "A Title" {
		t.Fatalf("GetBook: %+v %v", got, err)
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

func TestScratchBytesRoundTripAndSum(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	a, _ := db.CreateBook(ctx, NewBook{SourcePath: "/s/a", WorkDir: "/w/a", Title: "A"})
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/s/b", WorkDir: "/w/b", Title: "B"})

	// A fresh book accounts zero scratch, so the daemon total starts at zero.
	if a.ScratchBytes != 0 {
		t.Errorf("new book scratch_bytes = %d, want 0", a.ScratchBytes)
	}
	if sum, err := db.SumScratchBytes(ctx); err != nil || sum != 0 {
		t.Fatalf("SumScratchBytes(fresh) = %d,%v, want 0,nil", sum, err)
	}

	// The write side (split / purge) records a size; reads then serve the column.
	if err := db.UpdateScratchBytes(ctx, a.ID, 1500); err != nil {
		t.Fatalf("UpdateScratchBytes: %v", err)
	}
	if err := db.UpdateScratchBytes(ctx, b.ID, 500); err != nil {
		t.Fatalf("UpdateScratchBytes: %v", err)
	}
	got, _ := db.GetBook(ctx, a.ID)
	if got.ScratchBytes != 1500 {
		t.Errorf("GetBook scratch_bytes = %d, want 1500", got.ScratchBytes)
	}
	if sum, err := db.SumScratchBytes(ctx); err != nil || sum != 2000 {
		t.Errorf("SumScratchBytes = %d,%v, want 2000,nil", sum, err)
	}

	// A missing id is reported.
	if err := db.UpdateScratchBytes(ctx, 9999, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateScratchBytes(missing) = %v, want ErrNotFound", err)
	}

	// A scratch-gauge write must NOT bump updated_at: it is disk bookkeeping, not a
	// pipeline-state change, and must not reorder the Running list.
	before, _ := db.GetBook(ctx, a.ID)
	if err := db.UpdateScratchBytes(ctx, a.ID, 4242); err != nil {
		t.Fatalf("UpdateScratchBytes: %v", err)
	}
	after, _ := db.GetBook(ctx, a.ID)
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("UpdateScratchBytes bumped updated_at: %q -> %q", before.UpdatedAt, after.UpdatedAt)
	}
	if after.ScratchBytes != 4242 {
		t.Errorf("scratch_bytes = %d, want 4242", after.ScratchBytes)
	}
}

func TestSetBookPipelineStateLeavesStatusAndError(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/p", WorkDir: "/w", Title: "P"})
	// Put the book in a paused-with-error condition.
	if err := db.SetBookStatus(ctx, b.ID, "paused", "boom"); err != nil {
		t.Fatal(err)
	}
	// A pipeline-state advance must move ONLY state, leaving status and error intact.
	if err := db.SetBookPipelineState(ctx, b.ID, "splitting"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetBook(ctx, b.ID)
	if got.State != "splitting" {
		t.Errorf("state = %q, want splitting", got.State)
	}
	if got.Status != "paused" {
		t.Errorf("status = %q, want paused (not clobbered)", got.Status)
	}
	if got.Error != "boom" {
		t.Errorf("error = %q, want boom (not wiped)", got.Error)
	}
	// A missing id is reported.
	if err := db.SetBookPipelineState(ctx, 9999, "asr"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing id = %v, want ErrNotFound", err)
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
	allSucc, err := db.SucceededStagesAll(ctx)
	if err != nil {
		t.Fatalf("SucceededStagesAll: %v", err)
	}
	if !allSucc[b.ID]["asr"] {
		t.Fatalf("SucceededStagesAll = %+v", allSucc)
	}
	if err := db.DeleteStageSuccess(ctx, b.ID, "asr"); err != nil {
		t.Fatal(err)
	}
	allSucc, _ = db.SucceededStagesAll(ctx)
	if allSucc[b.ID]["asr"] {
		t.Fatalf("DeleteStageSuccess did not remove: %+v", allSucc[b.ID])
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
	all, _ := db.ListProgress(ctx, b.ID)
	if len(all) != 1 || all[0].Done != 5 || all[0].Total != 10 {
		t.Fatalf("ListProgress = %+v", all)
	}

	// A second book with its own progress, to exercise the bucketed query.
	b2, _ := db.CreateBook(ctx, NewBook{SourcePath: "/y", WorkDir: "/w2", Title: "Y"})
	if err := db.SetProgress(ctx, b2.ID, "splitting", 1, 3); err != nil {
		t.Fatal(err)
	}
	byBook, err := db.ListAllProgress(ctx)
	if err != nil {
		t.Fatalf("ListAllProgress: %v", err)
	}
	if len(byBook) != 2 {
		t.Fatalf("ListAllProgress buckets = %d, want 2", len(byBook))
	}
	if len(byBook[b.ID]) != 1 || byBook[b.ID][0].Stage != "asr" {
		t.Fatalf("bucket for b = %+v", byBook[b.ID])
	}
	if len(byBook[b2.ID]) != 1 || byBook[b2.ID][0].Stage != "splitting" {
		t.Fatalf("bucket for b2 = %+v", byBook[b2.ID])
	}
}

func TestEventsLogAndPrune(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// One recent, one old.
	if err := db.InsertEvent(ctx, now, 42, "book.state", 7, json.RawMessage(`{"state":"asr"}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertEvent(ctx, now.Add(-40*24*time.Hour), 7, "queue.stats", 0, nil); err != nil {
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
	// The SSE hub id round-trips into the durable log.
	if byBook[0].HubID != 42 {
		t.Fatalf("hub_id = %d, want 42", byBook[0].HubID)
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

// TestAuthStoreReopenPersists is the real reopen regression: a file-backed store
// keeps the provisioned admin, its sessions, and its password across a Close/Open
// cycle (the durable-auth-in-SQLite guarantee the M1 migration off the JSON files
// established). The auth package itself is storage-agnostic, so this lives here.
func TestAuthStoreReopenPersists(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "sidecars.db")

	db, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mgr := auth.New(db.AuthStore())
	pw, err := mgr.EnsureAdmin()
	if err != nil || pw == "" {
		t.Fatalf("EnsureAdmin: pw=%q err=%v", pw, err)
	}
	tok, err := mgr.Login(pw)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	mgr2 := auth.New(db2.AuthStore())

	// The admin is not re-provisioned (no new one-time password).
	if pw2, _ := mgr2.EnsureAdmin(); pw2 != "" {
		t.Errorf("EnsureAdmin re-provisioned after reopen: %q", pw2)
	}
	// The session still resolves.
	if ok, _ := mgr2.Resolve(tok); !ok {
		t.Error("session did not survive reopen")
	}
	// The password still verifies (login succeeds).
	if _, err := mgr2.Login(pw); err != nil {
		t.Errorf("password did not survive reopen: %v", err)
	}
}

func TestTimestampFixedWidthLexicographic(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// A moment 500ms later. With time.RFC3339Nano the earlier value renders with no
	// fraction ("...00Z") and sorts AFTER "...00.5Z"; the fixed-width layout keeps
	// lexicographic == chronological.
	half := base.Add(500 * time.Millisecond)
	if timestamp(base) >= timestamp(half) {
		t.Fatalf("fixed-width timestamps not chronological: %q vs %q", timestamp(base), timestamp(half))
	}
}

func TestDeriveWorkDir(t *testing.T) {
	root := filepath.Join("data", "work")

	// Distinct source paths yield distinct dirs even when the title is identical.
	a := DeriveWorkDir(root, "/books/one", "Same Title")
	b := DeriveWorkDir(root, "/books/two", "Same Title")
	if a == b {
		t.Fatalf("distinct sources collided: %q == %q", a, b)
	}

	// An all-symbol/empty title falls back to the "book" slug.
	sym := filepath.Base(DeriveWorkDir(root, "/x", "!!!@@@###"))
	if !strings.HasPrefix(sym, "book-") {
		t.Errorf("all-symbol title fallback = %q, want book- prefix", sym)
	}
	empty := filepath.Base(DeriveWorkDir(root, "/y", "   "))
	if !strings.HasPrefix(empty, "book-") {
		t.Errorf("empty title fallback = %q, want book- prefix", empty)
	}

	// A very long title is truncated to a bounded path component.
	long := filepath.Base(DeriveWorkDir(root, "/z", strings.Repeat("abcd ", 60)))
	if len(long) > 60 {
		t.Errorf("long title not truncated: %d chars (%q)", len(long), long)
	}

	// Same input is deterministic.
	if DeriveWorkDir(root, "/books/one", "Same Title") != a {
		t.Error("DeriveWorkDir is not deterministic")
	}
}
