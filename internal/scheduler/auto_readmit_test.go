package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

// parkBook drives a book to a parked (needs_attention) agent stage carrying code and a
// scheduled retry_at, mirroring what the scheduler's setStatus writes on a timed park.
func parkBook(t *testing.T, s *Scheduler, id int64, code state.ParkCode, retryAt string) {
	t.Helper()
	ctx := context.Background()
	if err := s.db.SetBookState(ctx, id, string(state.FactPass), "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetBookStatusRetry(ctx, id, string(state.StatusNeedsAttention), "parked", string(code), retryAt); err != nil {
		t.Fatal(err)
	}
}

// TestAutoReadmitDueReadmitsTransientPark: a book parked agent_rate_limited with a past
// retry_at is re-admitted (status cleared) by the timed self-resume; the readmit drops a
// durable "auto-retry" note.
func TestAutoReadmitDueReadmitsTransientPark(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "due", "", "")
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)

	_, sub := h.hub.Subscribe(0)
	defer sub.Close()

	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	parkBook(t, s, b.ID, state.ParkAgentRateLimited, past)

	s.autoReadmitDue(context.Background())

	cur, _ := db.GetBook(context.Background(), b.ID)
	if cur.Status != "" || cur.ParkCode != "" || cur.RetryAt != "" {
		t.Fatalf("after auto-readmit: status=%q park_code=%q retry_at=%q, want all empty",
			cur.Status, cur.ParkCode, cur.RetryAt)
	}
	if cur.State != string(state.FactPass) {
		t.Errorf("state = %q, want the parked stage preserved (fact_pass)", cur.State)
	}
}

// TestAutoReadmitDueSkipsNotDueAndNonAuto: a future retry_at, an empty retry_at
// (pre-migration park), and a non-auto park code are all left parked.
func TestAutoReadmitDueSkipsNotDueAndNonAuto(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	ctx := context.Background()

	future := h.addBook(t, db, "future", "", "")
	parkBook(t, s, future.ID, state.ParkAgentUnavailable, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))

	legacy := h.addBook(t, db, "legacy", "", "") // parked before the feature: retry_at=''
	parkBook(t, s, legacy.ID, state.ParkAgentRateLimited, "")

	// A DUE retry_at but a code the timed resume does not own (defensive: budget parks
	// never set retry_at, but the code filter must still exclude them).
	wrongCode := h.addBook(t, db, "wrong", "", "")
	parkBook(t, s, wrongCode.ID, state.ParkBudgetExceeded, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))

	s.autoReadmitDue(ctx)

	for _, id := range []int64{future.ID, legacy.ID, wrongCode.ID} {
		cur, _ := db.GetBook(ctx, id)
		if cur.Status != string(state.StatusNeedsAttention) {
			t.Errorf("book %d status = %q, want still needs_attention", id, cur.Status)
		}
	}
}
