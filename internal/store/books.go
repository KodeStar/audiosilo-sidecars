package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// workDirSlugMax caps the human-readable slug portion of a derived work dir, so a
// very long title cannot produce an unwieldy path component.
const workDirSlugMax = 48

// statusNeedsAttention is the one status value that may carry a park code. The
// store treats State/Status as opaque strings (internal/state owns their meaning),
// but the park_code invariant - "a code is set ONLY while status is needs_attention"
// - is enforced here at the write so it can never be violated regardless of caller
// ceremony. Kept as a local literal rather than importing internal/state, preserving
// the store's opaque-string decoupling; it must match state.StatusNeedsAttention.
const statusNeedsAttention = "needs_attention"

// enforceParkCode drops a park code that does not belong to the status being written:
// park_code is meaningful only alongside needs_attention, so any other status writes
// an empty code. Centralized so SetBookState/SetBookStatus can't drift.
func enforceParkCode(status, parkCode string) string {
	if status != statusNeedsAttention {
		return ""
	}
	return parkCode
}

// enforceRetryAt drops a scheduled-retry instant that does not belong to the status
// being written: retry_at is meaningful only alongside needs_attention (the auto-readmit
// window), so any other status - including a clear via retry/resume - writes empty.
// Shares the park_code invariant so a status-clearing write can never leave a stale time.
func enforceRetryAt(status, retryAt string) string {
	if status != statusNeedsAttention {
		return ""
	}
	return retryAt
}

// DeriveWorkDir returns a unique per-book scratch directory under root. The
// source_path hash guarantees uniqueness (two books may share a title), while the
// title slug keeps the path human-readable. An empty/all-symbol title falls back
// to "book"; the slug is length-capped. It lives here (next to CreateBook/NewBook)
// so the identity-derivation logic is unit-testable without the transport layer.
func DeriveWorkDir(root, sourcePath, title string) string {
	sum := sha256.Sum256([]byte(sourcePath))
	name := slug(title)
	if name == "" {
		name = "book"
	}
	return filepath.Join(root, name+"-"+hex.EncodeToString(sum[:])[:8])
}

// slug lowercases and hyphenates a title into a filesystem-safe, length-capped
// directory-name fragment.
func slug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if b.Len() >= workDirSlugMax {
			break
		}
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// ErrNotFound is returned when a lookup by id/path finds no row.
var ErrNotFound = errors.New("not found")

// ErrDuplicate is returned by CreateBook when source_path already exists.
var ErrDuplicate = errors.New("duplicate source_path")

// Book is a persisted pipeline book. authors/identity_sources/coverage are
// decoded from their JSON columns. State/Status are opaque strings here (the
// store never interprets them); internal/state owns their meaning.
type Book struct {
	ID         int64
	BatchID    string
	SourcePath string
	WorkDir    string
	Title      string
	Authors    []string
	// Narrators holds the scan's narrator credits (JSON-array column, like Authors);
	// used by the contributing stage to compose a core add-work proposal.
	Narrators       []string
	Series          string
	SeriesPos       string
	ASIN            string
	ISBN            string
	IdentitySources map[string]string
	// WorkID is the meta.audiosilo.app work this book was matched to at enqueue time
	// (from its coverage verdict or a manual match), advisory enrichment keyed off
	// the path identity. Empty when unmatched.
	WorkID   string
	State    string
	Status   string
	Error    string
	Coverage json.RawMessage // '' in the DB decodes to nil
	// ScratchBytes is the accounted on-disk size of the book's work dir, written by
	// the split stage and PurgeScratch so reads never have to walk the dir.
	ScratchBytes int64
	// Chapters is the manifest chapter count, written by the pipeline right after
	// inspect succeeds (0 = not yet known). The ETA engine reads it as the per-book
	// chapter total for the per-chapter stages.
	Chapters int
	// DurationSec is the book's total audio duration in seconds, written by the
	// pipeline right after inspect succeeds (0 = not yet known / pre-inspect). It
	// rides on the book view so the Running list can show each book's length.
	DurationSec float64
	// ParkCode is the typed park reason (empty = none): set when status becomes
	// needs_attention, cleared whenever status clears. It rides beside Error.
	ParkCode string
	// RetryAt is an RFC3339 (UTC) instant at which the scheduler may auto-readmit a book
	// parked on a transient agent condition (empty = no scheduled retry). Like ParkCode
	// it is meaningful only alongside needs_attention and is cleared whenever status
	// clears. A book that predates migration 0008 (or a plain park) has '' and never
	// auto-readmits - only a human Retry re-admits it.
	RetryAt   string
	CreatedAt string
	UpdatedAt string
}

// BookTracking is the small, read-only projection needed to mark library-scan
// candidates that are already persisted. SourcePath is the durable identity;
// state/status let the UI distinguish completed work from active or exceptional
// queue entries without loading every book's JSON metadata on each scan poll.
type BookTracking struct {
	ID         int64
	SourcePath string
	State      string
	Status     string
}

// NewBook is the input to CreateBook: the identity/metadata fields a caller
// supplies. State defaults to "queued".
type NewBook struct {
	BatchID    string
	SourcePath string
	WorkDir    string
	Title      string
	Authors    []string
	// Narrators holds the scan's narrator credits (JSON-array column, like Authors).
	Narrators       []string
	Series          string
	SeriesPos       string
	ASIN            string
	ISBN            string
	IdentitySources map[string]string
	// WorkID is the matched meta.audiosilo.app work id (advisory; empty when unmatched).
	WorkID string
	// Coverage is the advisory metadata-coverage snapshot captured at scan time
	// (empty when unknown). It is stored as-is and returned on the book view.
	Coverage json.RawMessage
	State    string // "" defaults to queued
}

func isUniqueViolation(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		return serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE ||
			serr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}

func marshalJSON(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// marshalJSONDefault marshals v to a JSON string, substituting fallback when v
// marshals empty (a nil slice/map), so a JSON-array/object column stores "[]"/"{}"
// rather than "". Shared by CreateBook's authors/narrators/identity_sources encoding.
func marshalJSONDefault(v any, fallback string) (string, error) {
	s, err := marshalJSON(v)
	if err != nil {
		return "", err
	}
	if s == "" {
		return fallback, nil
	}
	return s, nil
}

// CreateBook inserts a queued book. It returns ErrDuplicate if source_path is
// already tracked, so the API can report a per-item conflict.
func (db *DB) CreateBook(ctx context.Context, nb NewBook) (Book, error) {
	authors, err := marshalJSONDefault(nb.Authors, "[]")
	if err != nil {
		return Book{}, err
	}
	narrators, err := marshalJSONDefault(nb.Narrators, "[]")
	if err != nil {
		return Book{}, err
	}
	idsrc, err := marshalJSONDefault(nb.IdentitySources, "{}")
	if err != nil {
		return Book{}, err
	}
	st := nb.State
	if st == "" {
		st = "queued"
	}
	batchID := nb.BatchID
	if batchID == "" {
		batchID = LegacyBatchID
	}
	now := timestamp(nowFn())
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO books
		 (batch_id, source_path, work_dir, title, authors, narrators, series, series_pos, asin, isbn,
		  identity_sources, work_id, state, status, error, coverage, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,'','',?,?,?)`,
		batchID, nb.SourcePath, nb.WorkDir, nb.Title, authors, narrators, nb.Series, nb.SeriesPos,
		nb.ASIN, nb.ISBN, idsrc, nb.WorkID, st, string(nb.Coverage), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return Book{}, ErrDuplicate
		}
		return Book{}, fmt.Errorf("insert book: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Book{}, err
	}
	// Build the returned book from the input + assigned id/timestamps instead of a
	// second round-trip SELECT: the row was just inserted from exactly these values.
	return Book{
		ID:              id,
		BatchID:         batchID,
		SourcePath:      nb.SourcePath,
		WorkDir:         nb.WorkDir,
		Title:           nb.Title,
		Authors:         nb.Authors,
		Narrators:       nb.Narrators,
		Series:          nb.Series,
		SeriesPos:       nb.SeriesPos,
		ASIN:            nb.ASIN,
		ISBN:            nb.ISBN,
		IdentitySources: nb.IdentitySources,
		WorkID:          nb.WorkID,
		State:           st,
		Coverage:        nb.Coverage,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

const bookCols = `id, batch_id, source_path, work_dir, title, authors, narrators, series, series_pos,
	asin, isbn, identity_sources, work_id, state, status, error, coverage, scratch_bytes,
	chapters, duration_sec, park_code, retry_at, created_at, updated_at`

func scanBook(sc interface{ Scan(...any) error }) (Book, error) {
	var b Book
	var authors, narrators, idsrc, coverage string
	if err := sc.Scan(&b.ID, &b.BatchID, &b.SourcePath, &b.WorkDir, &b.Title, &authors, &narrators,
		&b.Series, &b.SeriesPos, &b.ASIN, &b.ISBN, &idsrc, &b.WorkID, &b.State, &b.Status,
		&b.Error, &coverage, &b.ScratchBytes, &b.Chapters, &b.DurationSec, &b.ParkCode,
		&b.RetryAt, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return Book{}, err
	}
	if authors != "" {
		if err := json.Unmarshal([]byte(authors), &b.Authors); err != nil {
			return Book{}, fmt.Errorf("decode authors: %w", err)
		}
	}
	if narrators != "" {
		if err := json.Unmarshal([]byte(narrators), &b.Narrators); err != nil {
			return Book{}, fmt.Errorf("decode narrators: %w", err)
		}
	}
	if idsrc != "" {
		if err := json.Unmarshal([]byte(idsrc), &b.IdentitySources); err != nil {
			return Book{}, fmt.Errorf("decode identity_sources: %w", err)
		}
	}
	if coverage != "" {
		b.Coverage = json.RawMessage(coverage)
	}
	return b, nil
}

// GetBook returns the book with id, or ErrNotFound.
func (db *DB) GetBook(ctx context.Context, id int64) (Book, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT `+bookCols+` FROM books WHERE id = ?`, id)
	b, err := scanBook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Book{}, ErrNotFound
	}
	return b, err
}

// ListBooks returns all books ordered by id.
func (db *DB) ListBooks(ctx context.Context) ([]Book, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+bookCols+` FROM books ORDER BY id`)
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

// ListBookTracking returns every persisted book keyed by its unique source path.
// It deliberately selects only the four fields the Library scan join needs: scan
// polling can be frequent, and decoding the full authors/coverage JSON for the
// entire queue on every poll would be wasted work.
func (db *DB) ListBookTracking(ctx context.Context) (map[string]BookTracking, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT id, source_path, state, status FROM books`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]BookTracking{}
	for rows.Next() {
		var tracked BookTracking
		if err := rows.Scan(&tracked.ID, &tracked.SourcePath, &tracked.State, &tracked.Status); err != nil {
			return nil, err
		}
		out[tracked.SourcePath] = tracked
	}
	return out, rows.Err()
}

// ListBooksDueForRetry returns every needs_attention book whose scheduled auto-readmit
// instant (retry_at) is set and at or before now (an RFC3339 UTC string, the same format
// SetBookStatusRetry writes, so a lexical compare is chronological). The scheduler
// filters these by park_code (only the transient agent codes auto-readmit); a book that
// predates migration 0008 has retry_at=” and is excluded here, so it never auto-readmits.
func (db *DB) ListBooksDueForRetry(ctx context.Context, now string) ([]Book, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+bookCols+` FROM books
		 WHERE status=? AND retry_at != '' AND retry_at <= ? ORDER BY id`,
		statusNeedsAttention, now)
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

// SetBookState updates a book's state, status, error, and typed park code together
// (the fields the scheduler mutates on every transition) and bumps updated_at. The
// park_code invariant is enforced here: a code is written only alongside a
// needs_attention status, so a status-clearing write always wipes any prior code
// even if the caller passes one.
func (db *DB) SetBookState(ctx context.Context, id int64, state, status, errMsg, parkCode string) error {
	// retry_at is preserved across a needs_attention-preserving write (a crash-reconcile
	// rewind of a timed park must keep its auto-readmit instant) and cleared otherwise (a
	// waypoint advance to a running/clear status), mirroring the park_code invariant. A
	// fresh timed park is stamped via SetBookStatusRetry, not here.
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET state=?, status=?, error=?, park_code=?,
		 retry_at = CASE WHEN ?=? THEN retry_at ELSE '' END,
		 updated_at=? WHERE id=?`,
		state, status, errMsg, enforceParkCode(status, parkCode),
		status, statusNeedsAttention, timestamp(nowFn()), id)
	return checkAffected(res, err)
}

// SetBookChapters records a book's manifest chapter count (written by the pipeline
// once inspect succeeds), which the ETA engine reads as the per-chapter stages'
// unit total. It is a pure gauge write and deliberately does NOT bump updated_at
// (mirroring UpdateScratchBytes): the chapter count is bookkeeping derived from the
// source, not a change to the book's pipeline position, and a spurious updated_at
// bump would reorder the Running list.
func (db *DB) SetBookChapters(ctx context.Context, id int64, chapters int) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET chapters=? WHERE id=?`, chapters, id)
	return checkAffected(res, err)
}

// SetBookDuration records a book's total audio duration in seconds (written by the
// pipeline once inspect succeeds), which the Running list shows as the book's
// length. Like SetBookChapters it is a pure gauge write and deliberately does NOT
// bump updated_at: the duration is bookkeeping derived from the source, not a
// change to the book's pipeline position, and a spurious updated_at bump would
// reorder the Running list.
func (db *DB) SetBookDuration(ctx context.Context, id int64, sec float64) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET duration_sec=? WHERE id=?`, sec, id)
	return checkAffected(res, err)
}

// SetBookPipelineState updates ONLY the pipeline state (and updated_at), leaving
// status and error untouched. The scheduler's advance() uses it: a normal forward
// transition must never clobber a status set concurrently (a pause/cancel landing
// in the window between a stage completing and its state advancing) nor wipe a
// prior error. Compare SetBookState, which sets state+status+error together.
func (db *DB) SetBookPipelineState(ctx context.Context, id int64, state string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET state=?, updated_at=? WHERE id=?`,
		state, timestamp(nowFn()), id)
	return checkAffected(res, err)
}

// SetBookStatus updates only the orthogonal status flag (paused/needs_attention/
// failed and back), preserving the pipeline state, and CLEARS any scheduled retry
// instant. The park_code invariant is enforced here: a code is written only alongside a
// needs_attention status, so any other status (including a clear) always wipes a prior
// code even if the caller passes one. It is the plain form of SetBookStatusRetry (no
// timed retry), so every existing caller (pause/resume/cancel/plain park) clears
// retry_at - a park that should auto-readmit uses SetBookStatusRetry instead.
func (db *DB) SetBookStatus(ctx context.Context, id int64, status, errMsg, parkCode string) error {
	return db.SetBookStatusRetry(ctx, id, status, errMsg, parkCode, "")
}

// SetBookStatusRetry is SetBookStatus plus the scheduled auto-readmit instant
// (books.retry_at, RFC3339 UTC): the scheduler stamps it when parking a book on a
// transient agent condition so a later dispatch pass can re-admit it once the window
// elapses. retry_at obeys the same needs_attention invariant as park_code (enforced
// here), so a status-clearing write wipes it even if a caller passes one.
func (db *DB) SetBookStatusRetry(ctx context.Context, id int64, status, errMsg, parkCode, retryAt string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET status=?, error=?, park_code=?, retry_at=?, updated_at=? WHERE id=?`,
		status, errMsg, enforceParkCode(status, parkCode), enforceRetryAt(status, retryAt),
		timestamp(nowFn()), id)
	return checkAffected(res, err)
}

// SetBookCoverage stores the coverage JSON snapshot for a book.
func (db *DB) SetBookCoverage(ctx context.Context, id int64, coverage json.RawMessage) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET coverage=?, updated_at=? WHERE id=?`,
		string(coverage), timestamp(nowFn()), id)
	return checkAffected(res, err)
}

// UpdateScratchBytes records a book's accounted on-disk scratch size (computed by
// a single DirSize walk at split completion / after a purge), so book-list and
// system reads serve it from the column without walking the work dir. It is a pure
// gauge write and deliberately does NOT bump updated_at: scratch size is bookkeeping
// about disk, not a change to the book's pipeline state, and a spurious updated_at
// bump would reorder the Running list (which sorts by created_at, but callers also
// use updated_at as a "last real change" signal).
func (db *DB) UpdateScratchBytes(ctx context.Context, id, bytes int64) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET scratch_bytes=? WHERE id=?`,
		bytes, id)
	return checkAffected(res, err)
}

// SumScratchBytes returns the daemon-total accounted scratch across all books -
// the /system disk gauge, served from the column with no filesystem walk.
func (db *DB) SumScratchBytes(ctx context.Context) (int64, error) {
	var total int64
	err := db.sql.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(scratch_bytes), 0) FROM books`).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// DeleteBook removes a book and (via ON DELETE CASCADE) its stage_runs/progress.
func (db *DB) DeleteBook(ctx context.Context, id int64) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM books WHERE id = ?`, id)
	return checkAffected(res, err)
}

// checkAffected maps a zero-row UPDATE/DELETE to ErrNotFound.
func checkAffected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
