package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// --- settings key/value ---

// GetSetting returns the value for key, or ("", false, nil) when absent.
func (db *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := db.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting upserts a key/value setting.
func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// --- durable event log ---

// InsertEvent appends a published event to the durable log. bookID <= 0 stores
// NULL (a daemon-wide event). payload is stored as-is (” becomes '{}').
func (db *DB) InsertEvent(ctx context.Context, ts time.Time, eventType string, bookID int64, payload json.RawMessage) error {
	p := string(payload)
	if p == "" {
		p = "{}"
	}
	var bid any
	if bookID > 0 {
		bid = bookID
	}
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO events (ts, type, book_id, payload) VALUES (?,?,?,?)`,
		timestamp(ts), eventType, bid, p)
	return err
}

// LoggedEvent is one row of the durable event log.
type LoggedEvent struct {
	ID      int64           `json:"id"`
	TS      string          `json:"ts"`
	Type    string          `json:"type"`
	BookID  *int64          `json:"book_id"`
	Payload json.RawMessage `json:"payload"`
}

// ListEvents returns up to limit most-recent events, optionally filtered to a
// book (bookID > 0). Newest first. Scaffolding for the M6 per-book log view (the
// SSE hub is the live feed; this reads the durable backlog).
func (db *DB) ListEvents(ctx context.Context, bookID int64, limit int) ([]LoggedEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	var (
		rows *sql.Rows
		err  error
	)
	if bookID > 0 {
		rows, err = db.sql.QueryContext(ctx,
			`SELECT id, ts, type, book_id, payload FROM events WHERE book_id=? ORDER BY id DESC LIMIT ?`,
			bookID, limit)
	} else {
		rows, err = db.sql.QueryContext(ctx,
			`SELECT id, ts, type, book_id, payload FROM events ORDER BY id DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LoggedEvent
	for rows.Next() {
		var e LoggedEvent
		var bid sql.NullInt64
		var payload string
		if err := rows.Scan(&e.ID, &e.TS, &e.Type, &bid, &payload); err != nil {
			return nil, err
		}
		if bid.Valid {
			v := bid.Int64
			e.BookID = &v
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents deletes events older than the cutoff and returns how many were
// removed. Called on startup to cap the durable log's growth.
func (db *DB) PruneEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := db.sql.ExecContext(ctx,
		`DELETE FROM events WHERE ts < ?`, timestamp(olderThan))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- rates (EWMA seed table; M1 create-only, minimal accessors) ---
//
// SetRate/GetRate are scaffolding for the M6 ETA engine (a per-stage EWMA
// unit-rate feeds the "time remaining" estimate). M1 only creates the table.

// SetRate upserts the unit-rate for a stage (seconds per unit).
func (db *DB) SetRate(ctx context.Context, stage string, unitRate float64) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO rates (stage, unit_rate, updated_at) VALUES (?,?,?)
		 ON CONFLICT(stage) DO UPDATE SET unit_rate=excluded.unit_rate, updated_at=excluded.updated_at`,
		stage, unitRate, timestamp(nowFn()))
	return err
}

// GetRate returns the unit-rate for a stage, or (0, false, nil) when unset.
func (db *DB) GetRate(ctx context.Context, stage string) (float64, bool, error) {
	var r float64
	err := db.sql.QueryRowContext(ctx, `SELECT unit_rate FROM rates WHERE stage=?`, stage).Scan(&r)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return r, true, nil
}
