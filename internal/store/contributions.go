package store

import (
	"context"
	"fmt"
	"slices"
)

// Contribution kind values (which artifact this row tracks). characters/recaps are
// the two sidecars; core is an add-work proposal opened when the book's work does not
// yet exist upstream.
const (
	ContribKindCharacters = "characters"
	ContribKindRecaps     = "recaps"
	ContribKindCore       = "core"
)

// Contribution mode values (how the artifact was contributed). Mirrors
// config.ContributionConfig.Mode.
const (
	ContribModeIssue = "issue"
	ContribModePR    = "pr"
	ContribModeLocal = "local"
)

// Contribution status values (the live state of a contributed artifact). submitted
// and pr_open are the OPEN states the poller advances; merged/closed are terminal;
// local is an export with no remote lifecycle; already_covered means the dimension
// already exists upstream so nothing was submitted.
const (
	ContribStatusSubmitted      = "submitted"
	ContribStatusPROpen         = "pr_open"
	ContribStatusMerged         = "merged"
	ContribStatusClosed         = "closed"
	ContribStatusLocal          = "local"
	ContribStatusAlreadyCovered = "already_covered"
)

// Contribution is one tracked contribution row: the state of one artifact (kind) for
// one book. Number/URL identify the created issue (issue mode) or PR (pr mode);
// PRNumber/PRURL track the intake bot PR an issue-mode contribution produces.
type Contribution struct {
	ID        int64
	BookID    int64
	Kind      string
	Mode      string
	Repo      string
	Number    int
	URL       string
	PRNumber  int
	PRURL     string
	Status    string
	Note      string
	CreatedAt string
	UpdatedAt string
}

const contribCols = `id, book_id, kind, mode, repo, number, url, pr_number, pr_url,
	status, note, created_at, updated_at`

func scanContribution(sc interface{ Scan(...any) error }) (Contribution, error) {
	var c Contribution
	if err := sc.Scan(&c.ID, &c.BookID, &c.Kind, &c.Mode, &c.Repo, &c.Number, &c.URL,
		&c.PRNumber, &c.PRURL, &c.Status, &c.Note, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Contribution{}, err
	}
	return c, nil
}

// UpsertContribution inserts a contribution row or, when one already exists for the
// same (book_id, kind), updates its mode/repo/number/url/status/note in place -
// preserving created_at (and the intake-PR fields, which only SetContributionStatus
// writes). This idempotency on (book, kind) is the resume guard: a stage that crashes
// between submitting and writing its sentinel re-runs and overwrites the same row
// rather than minting a duplicate. The CHECK constraints on kind/mode/status reject a
// bad value at the write. It returns the resulting row (with id + timestamps).
func (db *DB) UpsertContribution(ctx context.Context, c Contribution) (Contribution, error) {
	now := timestamp(nowFn())
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO contributions
		 (book_id, kind, mode, repo, number, url, status, note, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(book_id, kind) DO UPDATE SET
		   mode=excluded.mode, repo=excluded.repo, number=excluded.number,
		   url=excluded.url, status=excluded.status, note=excluded.note,
		   updated_at=excluded.updated_at`,
		c.BookID, c.Kind, c.Mode, c.Repo, c.Number, c.URL, c.Status, c.Note, now, now)
	if err != nil {
		return Contribution{}, fmt.Errorf("upsert contribution: %w", err)
	}
	return db.getContribution(ctx, c.BookID, c.Kind)
}

// getContribution reads the single row for (book_id, kind).
func (db *DB) getContribution(ctx context.Context, bookID int64, kind string) (Contribution, error) {
	row := db.sql.QueryRowContext(ctx,
		`SELECT `+contribCols+` FROM contributions WHERE book_id = ? AND kind = ?`, bookID, kind)
	return scanContribution(row)
}

// ListContributionsByBook returns all contribution rows for a book, ordered by kind
// (stable for the book detail view).
func (db *DB) ListContributionsByBook(ctx context.Context, bookID int64) ([]Contribution, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+contribCols+` FROM contributions WHERE book_id = ? ORDER BY kind`, bookID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Contribution
	for rows.Next() {
		c, err := scanContribution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ContributionsByBook returns every contribution row grouped by book id, so the
// book-list endpoint can attach each book's aggregate chip status in one query
// instead of N per-book reads. Rows within a book keep the kind ordering.
func (db *DB) ContributionsByBook(ctx context.Context) (map[int64][]Contribution, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+contribCols+` FROM contributions ORDER BY book_id, kind`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64][]Contribution)
	for rows.Next() {
		c, err := scanContribution(rows)
		if err != nil {
			return nil, err
		}
		out[c.BookID] = append(out[c.BookID], c)
	}
	return out, rows.Err()
}

// ListOpenContributions returns every contribution still in an OPEN state
// (submitted or pr_open) across all books - the poller's work list. Each row carries
// its book_id so the poller can advance the book without a second lookup.
func (db *DB) ListOpenContributions(ctx context.Context) ([]Contribution, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+contribCols+` FROM contributions
		 WHERE status IN (?, ?) ORDER BY id`,
		ContribStatusSubmitted, ContribStatusPROpen)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Contribution
	for rows.Next() {
		c, err := scanContribution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListBooksWithUnresolvedMergedCore returns the books that carry a merged kind=core
// contribution row but have no work_id yet - the poller's targeted work list for
// resolving a merged add-work PR's slug. It replaces a full-table ListBooks scan + a
// per-book ListContributionsByBook (an N+1): the WHERE-EXISTS join lets the poller fetch
// only the handful of books that actually need a slug resolved, and it drops a book out
// of the list as soon as its work_id is set (so an already-resolved book is not
// re-processed every tick), independent of the book's park state.
func (db *DB) ListBooksWithUnresolvedMergedCore(ctx context.Context) ([]Book, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+bookCols+` FROM books b
		 WHERE (b.work_id IS NULL OR b.work_id = '')
		   AND EXISTS (
		     SELECT 1 FROM contributions c
		     WHERE c.book_id = b.id AND c.kind = ? AND c.status = ?
		   )
		 ORDER BY b.id`,
		ContribKindCore, ContribStatusMerged)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetContributionStatus advances a contribution row's lifecycle: the poller uses it to
// record the discovered intake PR (prNumber/prURL) and move the status through
// pr_open/merged/closed, with an optional note. It bumps updated_at.
func (db *DB) SetContributionStatus(ctx context.Context, id int64, status string, prNumber int, prURL, note string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE contributions SET status=?, pr_number=?, pr_url=?, note=?, updated_at=? WHERE id=?`,
		status, prNumber, prURL, note, timestamp(nowFn()), id)
	return checkAffected(res, err)
}

// SetBookWorkID records the resolved upstream work slug on a book (the poller sets it
// once a core add-work PR merges, and the manual set-work endpoint sets it directly),
// so the contributing stage can attach the sidecars to the real work. It bumps
// updated_at.
func (db *DB) SetBookWorkID(ctx context.Context, id int64, workID string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET work_id=?, updated_at=? WHERE id=?`,
		workID, timestamp(nowFn()), id)
	return checkAffected(res, err)
}

// ContributionSummary folds a book's contribution rows into one chip status + the URL
// of the row that determined it. Precedence (most-actionable first): any closed ->
// closed; else any submitted -> submitted; else any pr_open -> pr_open; else all rows
// merged/already_covered -> merged; else all local -> local; no rows -> "" (the UI
// then shows the legacy "Local only"). A mixed set that fits no rung degrades to local.
// Pure - it takes rows and returns strings, so it is trivially unit-testable and can be
// called from the api layer without a store round-trip.
func ContributionSummary(rows []Contribution) (status, url string) {
	if len(rows) == 0 {
		return "", ""
	}
	if r, ok := firstWithStatus(rows, ContribStatusClosed); ok {
		return ContribStatusClosed, contribURL(r)
	}
	if r, ok := firstWithStatus(rows, ContribStatusSubmitted); ok {
		return ContribStatusSubmitted, contribURL(r)
	}
	if r, ok := firstWithStatus(rows, ContribStatusPROpen); ok {
		return ContribStatusPROpen, contribURL(r)
	}
	if allStatus(rows, ContribStatusMerged, ContribStatusAlreadyCovered) {
		if r, ok := firstWithStatus(rows, ContribStatusMerged); ok {
			return ContribStatusMerged, contribURL(r)
		}
		return ContribStatusMerged, "" // all already_covered
	}
	// all local, or any mixed remainder: least-committal chip.
	return ContribStatusLocal, ""
}

// contribURL picks a row's most relevant link: the intake/PR url when present, else
// the created issue/PR url. For a merged row this is "pr_url if set else url".
func contribURL(r Contribution) string {
	if r.PRURL != "" {
		return r.PRURL
	}
	return r.URL
}

func firstWithStatus(rows []Contribution, status string) (Contribution, bool) {
	for _, r := range rows {
		if r.Status == status {
			return r, true
		}
	}
	return Contribution{}, false
}

// allStatus reports whether every row's status is one of the allowed values.
func allStatus(rows []Contribution, allowed ...string) bool {
	for _, r := range rows {
		if !slices.Contains(allowed, r.Status) {
			return false
		}
	}
	return true
}
