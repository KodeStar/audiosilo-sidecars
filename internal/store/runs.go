package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// StageRun is one execution (or attempt) of a stage for a book. Ok is a nullable
// tri-state: nil = still running, false = failed/interrupted, true = completed.
//
// Model/InputTokens/OutputTokens/CostUSD (M5) capture agent spend: an agent stage
// accumulates them per invocation via AddOpenStageRunUsage. Mechanical/ASR stages
// leave them zero. CostUSD is 0 when the backend does not report a USD cost (codex).
//
// Superseded (migration 0008) splits the two questions a stage_runs row answers.
// SCHEDULING readers (round/fix-loop counters via CountStageSuccesses, the
// crash-resume "stage done" set via SucceededStages*) treat a superseded run as if it
// never happened: a Retry marks a stage's prior ok=1 runs superseded to reset those
// counters and force a fresh execution. MONEY readers (SumStageRunCost / StageRunTotals
// / the book-detail per-stage cost table via ListStageRuns) INCLUDE superseded rows -
// spend is spend, and a Retry must not erase a book's recorded cost. This replaced the
// old DeleteStageSuccess, which DELETED the rows and destroyed the cost history.
type StageRun struct {
	ID           int64           `json:"id"`
	BookID       int64           `json:"book_id"`
	Stage        string          `json:"stage"`
	Attempt      int             `json:"attempt"`
	StartedAt    string          `json:"started_at"`
	FinishedAt   string          `json:"finished_at"` // "" while running
	Ok           *bool           `json:"ok"`
	Metrics      json.RawMessage `json:"metrics"`
	Model        string          `json:"model"`
	InputTokens  int64           `json:"input_tokens"`
	OutputTokens int64           `json:"output_tokens"`
	CostUSD      float64         `json:"cost_usd"`
	// Superseded is true when a Retry reset this stage: the run's success no longer
	// counts for scheduling (round/fix counters, crash-resume set) but its cost still
	// counts. Failed runs are never superseded.
	Superseded bool `json:"superseded"`
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

// AddOpenStageRunUsage accumulates one agent invocation's token/cost usage onto the
// single OPEN run (finished_at IS NULL) for (book, stage). Token counts and cost add
// (a stage may invoke the agent several times - retries, per-chunk fact passes); the
// model column takes the LAST NON-EMPTY model. A failed/rate-limited invocation reports
// a zero Usage with Model="" (the backend never ran to report one), so overwriting the
// model unconditionally would erase the real model already recorded - the CASE keeps
// the prior value when the incoming model is empty. It must be called BEFORE the run is
// finished, so callers accumulate usage after each invocation and crash-preserve the
// spend. No open run for (book, stage) is a programming error (usage was reported
// outside a started run) and returns a descriptive error naming the book and stage.
func (db *DB) AddOpenStageRunUsage(ctx context.Context, bookID int64, stage, model string, in, out int64, cost float64) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE stage_runs
		 SET input_tokens = input_tokens + ?,
		     output_tokens = output_tokens + ?,
		     cost_usd = cost_usd + ?,
		     model = CASE WHEN ? <> '' THEN ? ELSE model END
		 WHERE book_id=? AND stage=? AND finished_at IS NULL`,
		in, out, cost, model, model, bookID, stage)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("AddOpenStageRunUsage: no open stage_run for book %d stage %q", bookID, stage)
	}
	return nil
}

// StageRunTotals returns the summed agent cost_usd per book across all stage runs,
// bucketed by book id, in ONE grouped query so the book-list endpoint attaches per-book
// totals without an N+1. Books with no runs (or only zero-cost mechanical/ASR runs) are
// absent from the map (callers read a missing key as 0). It is a MONEY reader, so it
// deliberately INCLUDES superseded runs - a Retry must not erase recorded spend.
func (db *DB) StageRunTotals(ctx context.Context) (map[int64]float64, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT book_id, COALESCE(SUM(cost_usd), 0) FROM stage_runs GROUP BY book_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]float64{}
	for rows.Next() {
		var bookID int64
		var total float64
		if err := rows.Scan(&bookID, &total); err != nil {
			return nil, err
		}
		out[bookID] = total
	}
	return out, rows.Err()
}

// SumStageRunCost returns the summed agent cost_usd across one book's stage runs - the
// single-book form of StageRunTotals for the book-detail/create paths. Like
// StageRunTotals it INCLUDES superseded runs (spend is spend); it is the number the
// per-book budget guard checks, so a Retry that supersedes a stage never lowers it.
func (db *DB) SumStageRunCost(ctx context.Context, bookID int64) (float64, error) {
	var total float64
	err := db.sql.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM stage_runs WHERE book_id=?`, bookID).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// StageRunStarts returns each book's earliest stage-run start (MIN(started_at))
// bucketed by book id, in ONE grouped query so the book-list endpoint attaches a
// per-book "started at" without an N+1. started_at is the store's fixed-width UTC
// form, so MIN is chronological. Books with no runs are absent from the map (a
// queued book that has not begun any stage yet).
func (db *DB) StageRunStarts(ctx context.Context) (map[int64]string, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT book_id, MIN(started_at) FROM stage_runs GROUP BY book_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var bookID int64
		var started string
		if err := rows.Scan(&bookID, &started); err != nil {
			return nil, err
		}
		out[bookID] = started
	}
	return out, rows.Err()
}

// FirstStageRunStart returns one book's earliest stage-run start - the single-book
// form of StageRunStarts for the book-detail/create paths. ok is false when the
// book has no runs yet (MIN over an empty set is NULL).
func (db *DB) FirstStageRunStart(ctx context.Context, bookID int64) (string, bool, error) {
	var started sql.NullString
	err := db.sql.QueryRowContext(ctx,
		`SELECT MIN(started_at) FROM stage_runs WHERE book_id=?`, bookID).Scan(&started)
	if err != nil {
		return "", false, err
	}
	if !started.Valid {
		return "", false, nil
	}
	return started.String, true, nil
}

// CountStageRuns returns how many runs of stage exist for a book (all attempts),
// used to compute the next attempt number and the fix-loop count.
func (db *DB) CountStageRuns(ctx context.Context, bookID int64, stage string) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stage_runs WHERE book_id=? AND stage=?`, bookID, stage).Scan(&n)
	return n, err
}

// CountStageSuccesses returns how many runs of stage completed ok AND are not
// superseded for a book. It is a SCHEDULING reader (round counters, the fix-loop
// count): a Retry marks a stage's successes superseded to reset the count, so a
// superseded success is excluded here (its cost still counts elsewhere).
func (db *DB) CountStageSuccesses(ctx context.Context, bookID int64, stage string) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stage_runs WHERE book_id=? AND stage=? AND ok=1 AND superseded=0`, bookID, stage).Scan(&n)
	return n, err
}

const runCols = `id, book_id, stage, attempt, started_at, finished_at, ok, metrics, ` +
	`model, input_tokens, output_tokens, cost_usd, superseded`

func scanRun(sc interface{ Scan(...any) error }) (StageRun, error) {
	var r StageRun
	var finished sql.NullString
	var ok sql.NullInt64
	var metrics string
	var superseded int64
	if err := sc.Scan(&r.ID, &r.BookID, &r.Stage, &r.Attempt, &r.StartedAt,
		&finished, &ok, &metrics, &r.Model, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &superseded); err != nil {
		return StageRun{}, err
	}
	r.Superseded = superseded == 1
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

// SucceededStagesAll returns, for every book, the set of stages with at least
// one ok=1 non-superseded run, in one grouped query - the "DB says done" set the
// reconcile cross-checks against on-disk sentinels. It is a SCHEDULING reader, so a
// superseded success (a Retry-reset stage) is excluded, exactly as the old
// DeleteStageSuccess removed the row. A single grouped query avoids a per-book N+1
// across the whole catalogue at startup; callers that want one book's set index the
// result by book id.
func (db *DB) SucceededStagesAll(ctx context.Context) (map[int64]map[string]bool, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT DISTINCT book_id, stage FROM stage_runs WHERE ok=1 AND superseded=0`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]map[string]bool{}
	for rows.Next() {
		var bookID int64
		var stage string
		if err := rows.Scan(&bookID, &stage); err != nil {
			return nil, err
		}
		set := out[bookID]
		if set == nil {
			set = map[string]bool{}
			out[bookID] = set
		}
		set[stage] = true
	}
	return out, rows.Err()
}

// SucceededStages returns one book's set of stages with at least one ok=1
// non-superseded run - the single-book form of SucceededStagesAll (a SCHEDULING
// reader), used by a mid-run reconcile (a scratch purge) that must recover just the
// purged book without a whole-catalogue scan.
func (db *DB) SucceededStages(ctx context.Context, bookID int64) (map[string]bool, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT DISTINCT stage FROM stage_runs WHERE book_id=? AND ok=1 AND superseded=0`, bookID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var stage string
		if err := rows.Scan(&stage); err != nil {
			return nil, err
		}
		out[stage] = true
	}
	return out, rows.Err()
}

// SupersedeStageSuccesses marks a stage's ok=1 runs superseded for a book, resetting
// the SCHEDULING counters (round/fix-loop counts, the crash-resume "done" set) so the
// stage re-runs, WITHOUT deleting the rows - their recorded cost is real spend and must
// survive. Used by Retry/auto-readmit and by reconcile when a completed stage's
// sentinel is missing and the stage must re-run. It replaced DeleteStageSuccess (which
// DELETED the rows and destroyed a book's cost history). Failed runs (ok=0) are left
// untouched.
func (db *DB) SupersedeStageSuccesses(ctx context.Context, bookID int64, stage string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE stage_runs SET superseded=1 WHERE book_id=? AND stage=? AND ok=1`, bookID, stage)
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

// ListProgress returns all progress rows for a book (used by the book-detail
// endpoint). The list endpoint uses ListAllProgress to avoid an N+1.
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

// ListAllProgress returns every book's progress rows bucketed by book id, in one
// query. The list endpoint uses it instead of calling ListProgress per book (an
// N+1). Books with no progress rows are simply absent from the map.
func (db *DB) ListAllProgress(ctx context.Context) (map[int64][]Progress, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT book_id, stage, done, total FROM progress ORDER BY book_id, stage`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64][]Progress{}
	for rows.Next() {
		var bookID int64
		var p Progress
		if err := rows.Scan(&bookID, &p.Stage, &p.Done, &p.Total); err != nil {
			return nil, err
		}
		out[bookID] = append(out[bookID], p)
	}
	return out, rows.Err()
}
