package store

import (
	"context"
	"encoding/json"
	"testing"
)

// findOverride returns the override for a source_path from the full list (there is
// no GetOverride; ListOverrides serves the list endpoint and callers use the
// UpsertOverride return value for the just-written row).
func findOverride(t *testing.T, db *DB, ctx context.Context, sourcePath string) (Override, bool) {
	t.Helper()
	list, err := db.ListOverrides(ctx)
	if err != nil {
		t.Fatalf("ListOverrides: %v", err)
	}
	for _, o := range list {
		if o.SourcePath == sourcePath {
			return o, true
		}
	}
	return Override{}, false
}

func TestOverridesCRUDAndDeleteOnEmpty(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	// Absent -> not found.
	if _, ok := findOverride(t, db, ctx, "/b/a"); ok {
		t.Fatal("override should be absent initially")
	}

	// A hide-only override persists, and the returned row carries updated_at (so
	// callers need no follow-up read).
	stored, err := db.UpsertOverride(ctx, Override{SourcePath: "/b/a", Hidden: true})
	if err != nil {
		t.Fatalf("UpsertOverride(hide): %v", err)
	}
	if !stored.Hidden || stored.UpdatedAt == "" {
		t.Fatalf("returned row = %+v, want hidden with updated_at", stored)
	}
	ov, ok := findOverride(t, db, ctx, "/b/a")
	if !ok || !ov.Hidden || ov.WorkID != "" {
		t.Fatalf("hide override = %+v ok=%v", ov, ok)
	}
	if ov.UpdatedAt == "" {
		t.Error("hide override missing updated_at")
	}

	// A manual match (not hidden) persists work_id + work_title.
	if _, err := db.UpsertOverride(ctx, Override{
		SourcePath: "/b/b", WorkID: "w-1", WorkTitle: "The Work",
	}); err != nil {
		t.Fatalf("UpsertOverride(match): %v", err)
	}
	ov, ok = findOverride(t, db, ctx, "/b/b")
	if !ok || ov.Hidden || ov.WorkID != "w-1" || ov.WorkTitle != "The Work" {
		t.Fatalf("match override = %+v", ov)
	}

	// A full-desired-state upsert replaces prior state (hidden -> matched).
	if _, err := db.UpsertOverride(ctx, Override{SourcePath: "/b/a", WorkID: "w-2", WorkTitle: "Two"}); err != nil {
		t.Fatal(err)
	}
	ov, _ = findOverride(t, db, ctx, "/b/a")
	if ov.Hidden || ov.WorkID != "w-2" {
		t.Fatalf("replaced override = %+v", ov)
	}

	// List returns both, ordered by source_path.
	list, err := db.ListOverrides(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListOverrides = %d %v", len(list), err)
	}
	if list[0].SourcePath != "/b/a" || list[1].SourcePath != "/b/b" {
		t.Fatalf("ListOverrides order = %+v", list)
	}

	// Delete-on-empty: an override that is neither hidden nor matched removes the row.
	if _, err := db.UpsertOverride(ctx, Override{SourcePath: "/b/a"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := findOverride(t, db, ctx, "/b/a"); ok {
		t.Fatal("empty upsert should have deleted the row")
	}
	// Deleting an absent override is a no-op, not an error.
	if _, err := db.UpsertOverride(ctx, Override{SourcePath: "/b/gone"}); err != nil {
		t.Fatalf("empty upsert of absent row: %v", err)
	}
}

func TestBookWorkIDRoundTrip(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, err := db.CreateBook(ctx, NewBook{
		SourcePath: "/b/w", WorkDir: "/w", Title: "W",
		WorkID: "work-42", Coverage: json.RawMessage(`{"available":true}`),
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if b.WorkID != "work-42" {
		t.Fatalf("created work_id = %q", b.WorkID)
	}
	got, err := db.GetBook(ctx, b.ID)
	if err != nil || got.WorkID != "work-42" {
		t.Fatalf("GetBook work_id = %q err=%v", got.WorkID, err)
	}
	// A book with no match stores an empty work_id, not NULL.
	b2, _ := db.CreateBook(ctx, NewBook{SourcePath: "/b/none", WorkDir: "/w2", Title: "None"})
	if b2.WorkID != "" {
		t.Fatalf("unmatched work_id = %q, want empty", b2.WorkID)
	}
}
