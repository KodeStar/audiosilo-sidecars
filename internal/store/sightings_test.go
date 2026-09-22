package store

import (
	"context"
	"testing"
	"time"
)

func TestSightingsRecordBaselineAndBump(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(48 * time.Hour)

	// Nothing recorded yet.
	has, err := db.HasSightings(ctx)
	if err != nil || has {
		t.Fatalf("HasSightings on an empty table = %v err=%v, want false", has, err)
	}

	// The first batch is the baseline: the library as it already stood.
	if err := db.RecordSightings(ctx, []string{"/lib/a", "/lib/b"}, t0, true); err != nil {
		t.Fatalf("RecordSightings(baseline): %v", err)
	}
	if has, err = db.HasSightings(ctx); err != nil || !has {
		t.Fatalf("HasSightings after the baseline = %v err=%v, want true", has, err)
	}
	rows, err := db.ListSightings(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ListSightings = %d rows, err=%v", len(rows), err)
	}
	if got := rows["/lib/a"]; !got.Baseline || got.FirstSeenAt != "2026-09-20T10:00:00Z" ||
		got.LastSeenAt != got.FirstSeenAt || got.AcknowledgedAt != "" {
		t.Fatalf("baseline row = %+v", got)
	}

	// A second batch: the known path keeps its first_seen_at AND its baseline flag
	// (re-scanning must never reset when a folder was first seen, and must never
	// promote a pre-existing book into the New view), while last_seen_at bumps. The
	// unknown path is recorded as NOT baseline - it is genuinely new.
	if err := db.RecordSightings(ctx, []string{"/lib/a", "/lib/c"}, t1, false); err != nil {
		t.Fatalf("RecordSightings(second pass): %v", err)
	}
	rows, err = db.ListSightings(ctx)
	if err != nil || len(rows) != 3 {
		t.Fatalf("ListSightings = %d rows, err=%v", len(rows), err)
	}
	kept := rows["/lib/a"]
	if !kept.Baseline || kept.FirstSeenAt != "2026-09-20T10:00:00Z" {
		t.Errorf("known path lost its first sighting: %+v", kept)
	}
	if kept.LastSeenAt != "2026-09-22T10:00:00Z" {
		t.Errorf("known path last_seen_at = %q, want the second pass", kept.LastSeenAt)
	}
	fresh := rows["/lib/c"]
	if fresh.Baseline || fresh.FirstSeenAt != "2026-09-22T10:00:00Z" {
		t.Errorf("newly-seen path = %+v, want a non-baseline sighting at t1", fresh)
	}
	// A path in NEITHER batch is untouched.
	if rows["/lib/b"].LastSeenAt != "2026-09-20T10:00:00Z" {
		t.Errorf("unseen path was bumped: %+v", rows["/lib/b"])
	}

	// An empty batch is a no-op, not an error.
	if err := db.RecordSightings(ctx, nil, t1, false); err != nil {
		t.Fatalf("RecordSightings(nil): %v", err)
	}
}

func TestAcknowledgeSightings(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 21, 8, 30, 0, 0, time.UTC)
	if err := db.RecordSightings(ctx, []string{"/lib/a", "/lib/b"}, at, false); err != nil {
		t.Fatal(err)
	}

	// Acknowledging stamps only the named path; an unknown path is ignored rather
	// than failing the call (a folder can vanish between scan and dismiss).
	if err := db.AcknowledgeSightings(ctx, []string{"/lib/a", "/lib/gone"}, at.Add(time.Hour)); err != nil {
		t.Fatalf("AcknowledgeSightings: %v", err)
	}
	rows, err := db.ListSightings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("unknown path was inserted: %+v", rows)
	}
	if got := rows["/lib/a"].AcknowledgedAt; got != "2026-09-21T09:30:00Z" {
		t.Errorf("acknowledged_at = %q, want the stamp", got)
	}
	if got := rows["/lib/b"].AcknowledgedAt; got != "" {
		t.Errorf("unrelated path acknowledged_at = %q, want empty", got)
	}

	// Re-recording a sighting for an acknowledged path must NOT un-dismiss it: a
	// rescan sees every folder again, and clearing the flag would resurrect the
	// whole library in the New view on the next scan.
	if err := db.RecordSightings(ctx, []string{"/lib/a"}, at.Add(2*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.ListSightings(ctx)
	if rows["/lib/a"].AcknowledgedAt == "" {
		t.Error("a rescan cleared an acknowledgement")
	}

	// Empty input is a no-op.
	if err := db.AcknowledgeSightings(ctx, nil, at); err != nil {
		t.Fatalf("AcknowledgeSightings(nil): %v", err)
	}
}
