package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// StageRun is one execution (or attempt) of a stage for a book. Ok is a nullable
// tri-state: nil = still running, false = failed/interrupted, true = completed.
type StageRun struct {
	ID         int64
	BookID     int64
	Stage      string
	Attempt    int
	StartedAt  string
	FinishedAt string // "" while running
	Ok         *bool
	Metrics    json.RawMessage
}

// StartStageRun opens a new run for (book, stage) with finished_at NULL and
// returns its id. attempt should be the 1-based count of prior runs of this
// stage + 1 (the caller computes it from CountStageRuns).
func (db *DB) StartStageRun(ctx context.Context, bookID int64, stage string, attempt int) (int64, error) {
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO stage_runs (book_id, stage, attempt, started_at, metrics)
		 VALUES (?,?,?,?, '{}')`,
		bookID, stage, attempt, timestamp(nowFn()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishStageRun closes a run, recording ok and optional metrics JSON.
func (db *DB) FinishStageRun(ctx context.Context, runID int64, ok bool, metrics json.RawMessage) error {
	m := string(metrics)
	if m == "" {
		m = "{}"
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE stage_runs SET finished_at=?, ok=?, metrics=? WHERE id=?`,
		timestamp(nowFn()), boolToInt(ok), m, runID)
	return checkAffected(res, err)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CountStageRuns returns how many runs of stage exist for a book (all attempts),
// used to compute the next attempt number and the fix-loop count.
func (db *DB) CountStageRuns(ctx context.Context, bookID int64, stage string) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stage_runs WHERE book_id=? AND stage=?`, bookID, stage).Scan(&n)
	return n, err
}

// CountStageSuccesses returns how many runs of stage completed ok for a book.
func (db *DB) CountStageSuccesses(ctx context.Context, bookID int64, stage string) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stage_runs WHERE book_id=? AND stage=? AND ok=1`, bookID, stage).Scan(&n)
	return n, err
}

const runCols = `id, book_id, stage, attempt, started_at, finished_at, ok, metrics`

func scanRun(sc interface{ Scan(...any) error }) (StageRun, error) {
	var r StageRun
	var finished sql.NullString
	var ok sql.NullInt64
	var metrics string
	if err := sc.Scan(&r.ID, &r.BookID, &r.Stage, &r.Attempt, &r.StartedAt,
		&finished, &ok, &metrics); err != nil {
		return StageRun{}, err
	}
	if finished.Valid {
		r.FinishedAt = finished.String
	}
	if ok.Valid {
		v := ok.Int64 == 1
		r.Ok = &v
	}
	if metrics != "" {
		r.Metrics = json.RawMessage(metrics)
	}
	return r, nil
}

// ListStageRuns returns every run for a book, oldest first.
func (db *DB) ListStageRuns(ctx context.Context, bookID int64) ([]StageRun, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+runCols+` FROM stage_runs WHERE book_id=? ORDER BY id`, bookID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StageRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpenStageRuns returns all runs still in flight (finished_at IS NULL) across all
// books - the set startup reconcile must close as interrupted.
func (db *DB) OpenStageRuns(ctx context.Context) ([]StageRun, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+runCols+` FROM stage_runs WHERE finished_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StageRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SucceededStages returns the distinct set of stages that have at least one ok=1
// run for a book - the "DB says done" set the reconcile cross-checks against
// on-disk sentinels.
func (db *DB) SucceededStages(ctx context.Context, bookID int64) (map[string]bool, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT DISTINCT stage FROM stage_runs WHERE book_id=? AND ok=1`, bookID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

// DeleteStageSuccess removes ok=1 runs of a stage for a book, used by reconcile
// when a completed stage's sentinel is missing and the stage must re-run.
func (db *DB) DeleteStageSuccess(ctx context.Context, bookID int64, stage string) error {
	_, err := db.sql.ExecContext(ctx,
		`DELETE FROM stage_runs WHERE book_id=? AND stage=? AND ok=1`, bookID, stage)
	return err
}

// --- progress ---

// Progress is the within-stage counter surfaced live in the UI.
type Progress struct {
	Stage string `json:"stage"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// SetProgress upserts the (book, stage) progress counter.
func (db *DB) SetProgress(ctx context.Context, bookID int64, stage string, done, total int) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO progress (book_id, stage, done, total) VALUES (?,?,?,?)
		 ON CONFLICT(book_id, stage) DO UPDATE SET done=excluded.done, total=excluded.total`,
		bookID, stage, done, total)
	return err
}

// GetProgress returns the counter for (book, stage), or (Progress{}, false).
func (db *DB) GetProgress(ctx context.Context, bookID int64, stage string) (Progress, bool, error) {
	var p Progress
	p.Stage = stage
	err := db.sql.QueryRowContext(ctx,
		`SELECT done, total FROM progress WHERE book_id=? AND stage=?`, bookID, stage).
		Scan(&p.Done, &p.Total)
	if errors.Is(err, sql.ErrNoRows) {
		return Progress{Stage: stage}, false, nil
	}
	if err != nil {
		return Progress{}, false, err
	}
	return p, true, nil
}

// ListProgress returns all progress rows for a book.
func (db *DB) ListProgress(ctx context.Context, bookID int64) ([]Progress, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT stage, done, total FROM progress WHERE book_id=? ORDER BY stage`, bookID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Progress
	for rows.Next() {
		var p Progress
		if err := rows.Scan(&p.Stage, &p.Done, &p.Total); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
