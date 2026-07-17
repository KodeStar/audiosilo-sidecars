package scheduler

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// TestRecordRate covers the RateSample path the EWMA rate update now feeds on: a stage
// that reports a sample with positive units and seconds folds its observed per-unit
// rate into the cache and persists it, while a nil or non-positive sample records
// nothing (the stage did no measurable work, or reported none).
func TestRecordRate(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, "")
	ctx := context.Background()

	// A sample of 10 chapters in 100s -> observed 10 s/chapter, blended with the asr
	// seed 36 via the EWMA (Alpha 0.3).
	s.recordRate(ctx, "asr", &RateSample{Units: 10, Seconds: 100})
	want := 0.3*10 + 0.7*36
	if got := s.rateSnapshot()["asr"]; math.Abs(got-want) > 1e-9 {
		t.Errorf("asr rate = %v, want %v", got, want)
	}
	// The rate was persisted, not just cached.
	persisted, err := db.ListRates(ctx)
	if err != nil {
		t.Fatalf("list rates: %v", err)
	}
	if math.Abs(persisted["asr"]-want) > 1e-9 {
		t.Errorf("persisted asr rate = %v, want %v", persisted["asr"], want)
	}

	// A nil sample records nothing.
	s.recordRate(ctx, "splitting", nil)
	if _, ok := s.rateSnapshot()["splitting"]; ok {
		t.Error("nil RateSample recorded a rate; want none")
	}
	// A non-positive sample (zero units or zero seconds) is likewise a no-op.
	s.recordRate(ctx, "splitting", &RateSample{Units: 0, Seconds: 100})
	s.recordRate(ctx, "splitting", &RateSample{Units: 5, Seconds: 0})
	if _, ok := s.rateSnapshot()["splitting"]; ok {
		t.Error("non-positive RateSample recorded a rate; want none")
	}
}

func TestRound10(t *testing.T) {
	cases := map[float64]int64{0: 0, -5: 0, 4: 0, 5: 10, 14: 10, 15: 20, 2401: 2400, 2405: 2410}
	for in, want := range cases {
		if got := round10(in); got != want {
			t.Errorf("round10(%v) = %d, want %d", in, got, want)
		}
	}
}

// TestPublishETAsAndDedup drives publishETAs directly: it publishes eta.update with
// per-book + queue estimates, the getters expose them, and a second identical pass
// is deduped (no new frame).
func TestPublishETAsAndDedup(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	hub := events.NewHub(64)
	s := New(db, hub, NewStubExecutor(0, 0), 2, "")
	s.ctx = context.Background()

	b, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: "/a", WorkDir: "/w/a", Title: "A",
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}

	_, sub := hub.Subscribe(32)
	defer sub.Close()

	books, _ := db.ListBooks(context.Background())
	s.publishETAs(context.Background(), books)

	// One eta.update frame carrying the queued book with a positive ETA.
	ev := waitFor(t, sub, "eta.update")
	var payload struct {
		QueueSeconds int64 `json:"queue_seconds"`
		Books        []struct {
			BookID     int64 `json:"book_id"`
			ETASeconds int64 `json:"eta_seconds"`
		} `json:"books"`
	}
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		t.Fatalf("unmarshal eta.update: %v", err)
	}
	if payload.QueueSeconds <= 0 {
		t.Errorf("queue_seconds = %d, want > 0", payload.QueueSeconds)
	}
	if len(payload.Books) != 1 || payload.Books[0].BookID != b.ID || payload.Books[0].ETASeconds <= 0 {
		t.Fatalf("eta.update books = %+v, want the one queued book with a positive ETA", payload.Books)
	}

	// The getter exposes the same per-book snapshot.
	if got, ok := s.ETASeconds(b.ID); !ok || got != payload.Books[0].ETASeconds {
		t.Errorf("ETASeconds = %d,%v, want %d,true", got, ok, payload.Books[0].ETASeconds)
	}

	// A second identical pass is deduped: no further eta.update frame.
	s.publishETAs(context.Background(), books)
	if got := drainType(sub, "eta.update"); got {
		t.Error("identical ETA snapshot republished; want deduped")
	}
}

// TestETASecondsNoETAForParked confirms a parked/terminal book has no getter entry.
func TestETASecondsNoETAForParked(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, "")
	s.ctx = context.Background()

	ctx := context.Background()
	active, _ := db.CreateBook(ctx, store.NewBook{SourcePath: "/a", WorkDir: "/w/a", Title: "A"})
	parked, _ := db.CreateBook(ctx, store.NewBook{SourcePath: "/b", WorkDir: "/w/b", Title: "B"})
	// Move the parked book into a needs_attention condition at a real stage.
	if err := db.SetBookState(ctx, parked.ID, "auditing", "needs_attention", "boom", "fix_loop_exhausted"); err != nil {
		t.Fatal(err)
	}

	books, _ := db.ListBooks(ctx)
	s.publishETAs(ctx, books)

	if _, ok := s.ETASeconds(active.ID); !ok {
		t.Error("active book has no ETA, want one")
	}
	if _, ok := s.ETASeconds(parked.ID); ok {
		t.Error("parked book has an ETA, want none")
	}
}

// TestPublishETAsIdleEmptySnapshot covers the idle gate: when no book is active
// (every book terminal/parked), publishETAs publishes a single empty eta.update and
// clears the stored snapshot, and a second idle pass is deduped.
func TestPublishETAsIdleEmptySnapshot(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	hub := events.NewHub(64)
	s := New(db, hub, NewStubExecutor(0, 0), 2, "")
	s.ctx = context.Background()
	ctx := context.Background()

	b, _ := db.CreateBook(ctx, store.NewBook{SourcePath: "/a", WorkDir: "/w/a", Title: "A"})

	// First pass: the queued book is active, so it gets an ETA.
	books, _ := db.ListBooks(ctx)
	s.publishETAs(ctx, books)
	if _, ok := s.ETASeconds(b.ID); !ok {
		t.Fatal("active book has no ETA after the first pass")
	}

	_, sub := hub.Subscribe(32)
	defer sub.Close()

	// Drive the only book terminal (done) -> no active book remains.
	if err := db.SetBookState(ctx, b.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	books, _ = db.ListBooks(ctx)
	s.publishETAs(ctx, books)

	// An empty eta.update is published and the snapshot is cleared. queue_seconds is
	// JSON null (nothing to estimate), not a numeric 0.
	ev := waitFor(t, sub, "eta.update")
	var payload struct {
		QueueSeconds *int64 `json:"queue_seconds"`
		Books        []any  `json:"books"`
	}
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		t.Fatalf("unmarshal eta.update: %v", err)
	}
	if payload.QueueSeconds != nil || len(payload.Books) != 0 {
		t.Fatalf("idle eta.update = %+v, want queue_seconds null and no books", payload)
	}
	if _, ok := s.ETASeconds(b.ID); ok {
		t.Error("done book still has an ETA after the idle clear")
	}
	if snap := s.ETASnapshot(); len(snap) != 0 {
		t.Errorf("cleared snapshot = %v, want empty", snap)
	}

	// A second idle pass is deduped: no further eta.update frame.
	s.publishETAs(ctx, books)
	if drainType(sub, "eta.update") {
		t.Error("idle clear republished; want deduped")
	}
}

// TestETASnapshot covers the single-lock snapshot getter: it returns the whole
// per-book map, and the returned map is a copy (mutating it does not affect the
// scheduler's stored snapshot).
func TestETASnapshot(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	s := New(db, events.NewHub(64), NewStubExecutor(0, 0), 2, "")
	s.ctx = context.Background()
	ctx := context.Background()

	b, _ := db.CreateBook(ctx, store.NewBook{SourcePath: "/a", WorkDir: "/w/a", Title: "A"})
	books, _ := db.ListBooks(ctx)
	s.publishETAs(ctx, books)

	snap := s.ETASnapshot()
	single, ok := s.ETASeconds(b.ID)
	if !ok || snap[b.ID] != single {
		t.Errorf("snapshot[%d] = %d, single getter = %d,%v", b.ID, snap[b.ID], single, ok)
	}
	// Mutating the returned copy must not affect the stored snapshot.
	snap[b.ID] = -1
	if again := s.ETASnapshot(); again[b.ID] != single {
		t.Errorf("stored snapshot mutated through the returned copy: %d", again[b.ID])
	}
}

// waitFor drains the subscription until it sees an event of the given type.
func waitFor(t *testing.T, sub *events.Subscription, typ string) events.Event {
	t.Helper()
	for ev := range sub.C {
		if ev.Type == typ {
			return ev
		}
	}
	t.Fatalf("subscription closed before a %q event", typ)
	return events.Event{}
}

// drainType reports whether any currently-buffered event of the given type is
// present (non-blocking).
func drainType(sub *events.Subscription, typ string) bool {
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return false
			}
			if ev.Type == typ {
				return true
			}
		default:
			return false
		}
	}
}
