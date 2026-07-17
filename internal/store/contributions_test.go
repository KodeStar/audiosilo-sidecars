package store

import (
	"context"
	"errors"
	"testing"
)

func TestNarratorsRoundTrip(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	b, err := db.CreateBook(ctx, NewBook{
		SourcePath: "/n/a", WorkDir: "/w/a", Title: "N",
		Authors: []string{"Auth One"}, Narrators: []string{"Nora Narrator", "Sam Speaker"},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if len(b.Narrators) != 2 || b.Narrators[0] != "Nora Narrator" {
		t.Fatalf("create narrators: %+v", b.Narrators)
	}
	got, _ := db.GetBook(ctx, b.ID)
	if len(got.Narrators) != 2 || got.Narrators[1] != "Sam Speaker" {
		t.Fatalf("get narrators: %+v", got.Narrators)
	}

	// A book with no narrators reads back an empty list (the '[]' column default), not
	// an error - the authors column's exact behavior.
	b2, err := db.CreateBook(ctx, NewBook{SourcePath: "/n/b", WorkDir: "/w/b", Title: "N2"})
	if err != nil {
		t.Fatalf("CreateBook(no narrators): %v", err)
	}
	got2, _ := db.GetBook(ctx, b2.ID)
	if len(got2.Narrators) != 0 {
		t.Fatalf("empty narrators = %+v, want none", got2.Narrators)
	}
}

// mustMergedCore upserts a kind=core row for a book and advances it to merged (with a
// PR pointer), the state the poller resolves a slug from.
func mustMergedCore(t *testing.T, db *DB, bookID int64) {
	t.Helper()
	ctx := context.Background()
	row, err := db.UpsertContribution(ctx, Contribution{
		BookID: bookID, Kind: ContribKindCore, Mode: ContribModeIssue, Status: ContribStatusSubmitted,
	})
	if err != nil {
		t.Fatalf("upsert core: %v", err)
	}
	if err := db.SetContributionStatus(ctx, row.ID, ContribStatusMerged, 5, "https://gh/pull/5", ""); err != nil {
		t.Fatalf("merge core: %v", err)
	}
}

func TestListBooksWithUnresolvedMergedCore(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	// A: merged core row, no work_id -> INCLUDED (needs resolution).
	a, _ := db.CreateBook(ctx, NewBook{SourcePath: "/u/a", WorkDir: "/w/a", Title: "A"})
	mustMergedCore(t, db, a.ID)

	// B: merged core row but work_id already set -> EXCLUDED (already resolved, so it
	// drops out of the poller's work list instead of being re-processed every tick).
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/u/b", WorkDir: "/w/b", Title: "B"})
	mustMergedCore(t, db, b.ID)
	if err := db.SetBookWorkID(ctx, b.ID, "resolved-work"); err != nil {
		t.Fatalf("set work id: %v", err)
	}

	// C: core row still submitted (not merged) -> EXCLUDED.
	c, _ := db.CreateBook(ctx, NewBook{SourcePath: "/u/c", WorkDir: "/w/c", Title: "C"})
	if _, err := db.UpsertContribution(ctx, Contribution{
		BookID: c.ID, Kind: ContribKindCore, Mode: ContribModeIssue, Status: ContribStatusSubmitted,
	}); err != nil {
		t.Fatalf("upsert submitted core: %v", err)
	}

	// D: no core row at all -> EXCLUDED.
	if _, err := db.CreateBook(ctx, NewBook{SourcePath: "/u/d", WorkDir: "/w/d", Title: "D"}); err != nil {
		t.Fatalf("create D: %v", err)
	}

	got, err := db.ListBooksWithUnresolvedMergedCore(ctx)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].ID != a.ID {
		var ids []int64
		for _, bk := range got {
			ids = append(ids, bk.ID)
		}
		t.Fatalf("got book ids %v, want only [%d]", ids, a.ID)
	}
}

func TestContributionsCRUDAndUpsert(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/c/a", WorkDir: "/w/a", Title: "A"})

	c1, err := db.UpsertContribution(ctx, Contribution{
		BookID: b.ID, Kind: ContribKindCharacters, Mode: ContribModeIssue,
		Repo: "KodeStar/audiosilo-meta", Number: 12, URL: "https://gh/issues/12",
		Status: ContribStatusSubmitted,
	})
	if err != nil {
		t.Fatalf("upsert characters: %v", err)
	}
	if c1.ID == 0 || c1.CreatedAt == "" || c1.UpdatedAt == "" {
		t.Fatalf("c1 not populated: %+v", c1)
	}
	if _, err := db.UpsertContribution(ctx, Contribution{
		BookID: b.ID, Kind: ContribKindRecaps, Mode: ContribModeIssue,
		Status: ContribStatusSubmitted,
	}); err != nil {
		t.Fatalf("upsert recaps: %v", err)
	}

	// Idempotent on (book, kind): re-upsert characters updates in place, same id, and
	// preserves created_at (the resume guard, so a crash never double-posts).
	c1b, err := db.UpsertContribution(ctx, Contribution{
		BookID: b.ID, Kind: ContribKindCharacters, Mode: ContribModeIssue,
		Number: 99, URL: "https://gh/issues/99", Status: ContribStatusSubmitted, Note: "relabelled",
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if c1b.ID != c1.ID {
		t.Errorf("upsert minted a new row: %d != %d", c1b.ID, c1.ID)
	}
	if c1b.CreatedAt != c1.CreatedAt {
		t.Errorf("created_at not preserved: %q -> %q", c1.CreatedAt, c1b.CreatedAt)
	}
	if c1b.Number != 99 || c1b.Note != "relabelled" || c1b.URL != "https://gh/issues/99" {
		t.Errorf("upsert did not update fields: %+v", c1b)
	}

	// List by book, ordered by kind (characters then recaps alphabetically).
	rows, err := db.ListContributionsByBook(ctx, b.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ListContributionsByBook: %+v %v", rows, err)
	}
	if rows[0].Kind != ContribKindCharacters || rows[1].Kind != ContribKindRecaps {
		t.Errorf("order = %q,%q", rows[0].Kind, rows[1].Kind)
	}

	// Open set: both are submitted, and each carries its book_id.
	open, err := db.ListOpenContributions(ctx)
	if err != nil || len(open) != 2 {
		t.Fatalf("ListOpenContributions: %+v %v", open, err)
	}
	for _, o := range open {
		if o.BookID != b.ID {
			t.Errorf("open row missing book_id: %+v", o)
		}
	}

	// Advance characters to merged via the intake PR -> it drops out of the open set.
	if err := db.SetContributionStatus(ctx, c1.ID, ContribStatusMerged, 500, "https://gh/pull/500", "merged upstream"); err != nil {
		t.Fatalf("SetContributionStatus: %v", err)
	}
	rows, _ = db.ListContributionsByBook(ctx, b.ID)
	chars := rows[0]
	if chars.Status != ContribStatusMerged || chars.PRNumber != 500 ||
		chars.PRURL != "https://gh/pull/500" || chars.Note != "merged upstream" {
		t.Fatalf("after status: %+v", chars)
	}
	open, _ = db.ListOpenContributions(ctx)
	if len(open) != 1 || open[0].Kind != ContribKindRecaps {
		t.Fatalf("open after merge = %+v", open)
	}
	if err := db.SetContributionStatus(ctx, 9999, ContribStatusClosed, 0, "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetContributionStatus(missing) = %v, want ErrNotFound", err)
	}

	// SetBookWorkID persists the resolved slug.
	if err := db.SetBookWorkID(ctx, b.ID, "the-slug"); err != nil {
		t.Fatalf("SetBookWorkID: %v", err)
	}
	bg, _ := db.GetBook(ctx, b.ID)
	if bg.WorkID != "the-slug" {
		t.Errorf("work_id = %q, want the-slug", bg.WorkID)
	}
	if err := db.SetBookWorkID(ctx, 9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetBookWorkID(missing) = %v, want ErrNotFound", err)
	}

	// Deleting the book cascades to its contribution rows.
	if err := db.DeleteBook(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.ListContributionsByBook(ctx, b.ID)
	if len(rows) != 0 {
		t.Fatalf("ON DELETE CASCADE left rows: %+v", rows)
	}
}

func TestContributionsCheckConstraints(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/c/x", WorkDir: "/w/x", Title: "X"})

	bad := map[string]Contribution{
		"bad kind":   {BookID: b.ID, Kind: "bogus", Mode: ContribModeIssue, Status: ContribStatusSubmitted},
		"bad mode":   {BookID: b.ID, Kind: ContribKindCharacters, Mode: "ftp", Status: ContribStatusSubmitted},
		"bad status": {BookID: b.ID, Kind: ContribKindRecaps, Mode: ContribModeIssue, Status: "weird"},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := db.UpsertContribution(ctx, c); err == nil {
				t.Errorf("CHECK constraint should reject %+v", c)
			}
		})
	}
}

func TestContributionSummary(t *testing.T) {
	cases := []struct {
		name    string
		rows    []Contribution
		want    string
		wantURL string
	}{
		{"none", nil, "", ""},
		{
			"closed wins over merged",
			[]Contribution{{Status: ContribStatusMerged, PRURL: "m"}, {Status: ContribStatusClosed, URL: "c"}},
			ContribStatusClosed, "c",
		},
		{
			"submitted over pr_open",
			[]Contribution{{Status: ContribStatusPROpen, PRURL: "p"}, {Status: ContribStatusSubmitted, URL: "s"}},
			ContribStatusSubmitted, "s",
		},
		{
			"pr_open over merged",
			[]Contribution{{Status: ContribStatusPROpen, PRURL: "p"}, {Status: ContribStatusMerged, PRURL: "m"}},
			ContribStatusPROpen, "p",
		},
		{
			"all merged/already_covered -> merged (pr_url preferred)",
			[]Contribution{{Status: ContribStatusMerged, PRURL: "mp", URL: "mu"}, {Status: ContribStatusAlreadyCovered}},
			ContribStatusMerged, "mp",
		},
		{
			"merged falls back to url when no pr_url",
			[]Contribution{{Status: ContribStatusMerged, URL: "onlyurl"}},
			ContribStatusMerged, "onlyurl",
		},
		{
			"all already_covered still reads merged",
			[]Contribution{{Status: ContribStatusAlreadyCovered}, {Status: ContribStatusAlreadyCovered}},
			ContribStatusMerged, "",
		},
		{
			"all local -> local",
			[]Contribution{{Status: ContribStatusLocal}, {Status: ContribStatusLocal}},
			ContribStatusLocal, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, url := ContributionSummary(tc.rows)
			if got != tc.want || url != tc.wantURL {
				t.Errorf("ContributionSummary = %q,%q, want %q,%q", got, url, tc.want, tc.wantURL)
			}
		})
	}
}
