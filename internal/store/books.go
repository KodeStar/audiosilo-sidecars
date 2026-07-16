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
	ID              int64
	SourcePath      string
	WorkDir         string
	Title           string
	Authors         []string
	Series          string
	SeriesPos       string
	ASIN            string
	ISBN            string
	IdentitySources map[string]string
	State           string
	Status          string
	Error           string
	Coverage        json.RawMessage // '' in the DB decodes to nil
	// ScratchBytes is the accounted on-disk size of the book's work dir, written by
	// the split stage and PurgeScratch so reads never have to walk the dir.
	ScratchBytes int64
	CreatedAt    string
	UpdatedAt    string
}

// NewBook is the input to CreateBook: the identity/metadata fields a caller
// supplies. State defaults to "queued".
type NewBook struct {
	SourcePath      string
	WorkDir         string
	Title           string
	Authors         []string
	Series          string
	SeriesPos       string
	ASIN            string
	ISBN            string
	IdentitySources map[string]string
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

// CreateBook inserts a queued book. It returns ErrDuplicate if source_path is
// already tracked, so the API can report a per-item conflict.
func (db *DB) CreateBook(ctx context.Context, nb NewBook) (Book, error) {
	authors, err := marshalJSON(nb.Authors)
	if err != nil {
		return Book{}, err
	}
	if authors == "" {
		authors = "[]"
	}
	idsrc, err := marshalJSON(nb.IdentitySources)
	if err != nil {
		return Book{}, err
	}
	if idsrc == "" {
		idsrc = "{}"
	}
	st := nb.State
	if st == "" {
		st = "queued"
	}
	now := timestamp(nowFn())
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO books
		 (source_path, work_dir, title, authors, series, series_pos, asin, isbn,
		  identity_sources, state, status, error, coverage, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,'','',?,?,?)`,
		nb.SourcePath, nb.WorkDir, nb.Title, authors, nb.Series, nb.SeriesPos,
		nb.ASIN, nb.ISBN, idsrc, st, string(nb.Coverage), now, now)
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
		SourcePath:      nb.SourcePath,
		WorkDir:         nb.WorkDir,
		Title:           nb.Title,
		Authors:         nb.Authors,
		Series:          nb.Series,
		SeriesPos:       nb.SeriesPos,
		ASIN:            nb.ASIN,
		ISBN:            nb.ISBN,
		IdentitySources: nb.IdentitySources,
		State:           st,
		Coverage:        nb.Coverage,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

const bookCols = `id, source_path, work_dir, title, authors, series, series_pos,
	asin, isbn, identity_sources, state, status, error, coverage, scratch_bytes,
	created_at, updated_at`

func scanBook(sc interface{ Scan(...any) error }) (Book, error) {
	var b Book
	var authors, idsrc, coverage string
	if err := sc.Scan(&b.ID, &b.SourcePath, &b.WorkDir, &b.Title, &authors,
		&b.Series, &b.SeriesPos, &b.ASIN, &b.ISBN, &idsrc, &b.State, &b.Status,
		&b.Error, &coverage, &b.ScratchBytes, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return Book{}, err
	}
	if authors != "" {
		if err := json.Unmarshal([]byte(authors), &b.Authors); err != nil {
			return Book{}, fmt.Errorf("decode authors: %w", err)
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

// SetBookState updates a book's state, status, and error together (the fields the
// scheduler mutates on every transition) and bumps updated_at.
func (db *DB) SetBookState(ctx context.Context, id int64, state, status, errMsg string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET state=?, status=?, error=?, updated_at=? WHERE id=?`,
		state, status, errMsg, timestamp(nowFn()), id)
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
// failed and back), preserving the pipeline state. errMsg is stored as-is
// (callers pass "" to clear it).
func (db *DB) SetBookStatus(ctx context.Context, id int64, status, errMsg string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET status=?, error=?, updated_at=? WHERE id=?`,
		status, errMsg, timestamp(nowFn()), id)
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
// system reads serve it from the column without walking the work dir.
func (db *DB) UpdateScratchBytes(ctx context.Context, id, bytes int64) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET scratch_bytes=?, updated_at=? WHERE id=?`,
		bytes, timestamp(nowFn()), id)
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
