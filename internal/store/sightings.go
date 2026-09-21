package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SightingLayout is the timestamp layout of every library_sightings column.
//
// It is plain RFC3339 (seconds, UTC) rather than the store's fixed-width
// nanosecond timeLayout because first_seen_at is served VERBATIM on the wire
// (ScannedBook.first_seen_at), and that contract pins RFC3339. All three columns
// share the one layout, so lexicographic compares remain chronological.
const SightingLayout = time.RFC3339

// Sighting is the record of a library folder having been observed by a completed
// folder scan. It is path-keyed durable state with no FK to the book index, so it
// survives an enqueue, a delete and a rescan.
type Sighting struct {
	SourcePath  string
	FirstSeenAt string
	LastSeenAt  string
	// AcknowledgedAt is empty while the user has not dismissed this book from the
	// Library tab's New view.
	AcknowledgedAt string
	// Baseline marks a path recorded by the FIRST batch ever recorded (the library
	// as it already stood). A baseline sighting is never new.
	Baseline bool
}

// sightingTime renders a moment in the sightings layout (always UTC).
func sightingTime(at time.Time) string { return at.UTC().Format(SightingLayout) }

// RecordSightings records every path as seen at `at`. A path that is already
// known keeps its first_seen_at and its baseline flag and only has last_seen_at
// bumped - first_seen_at is the whole point of the table, so re-scanning a
// library must never reset it.
//
// baseline stamps the inserted rows as "the library as it already stood". The
// CALLER decides that (the scan manager passes true only when the table is
// empty); this method just stores the answer, so the baseline rule stays in one
// place instead of being half-implemented in SQL.
func (db *DB) RecordSightings(ctx context.Context, paths []string, at time.Time, baseline bool) error {
	if len(paths) == 0 {
		return nil
	}
	ts := sightingTime(at)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO library_sightings (source_path, first_seen_at, last_seen_at, baseline)
		 VALUES (?,?,?,?)
		 ON CONFLICT(source_path) DO UPDATE SET last_seen_at=excluded.last_seen_at`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, p, ts, ts, boolToInt(baseline)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListSightings returns every sighting keyed by source_path. The whole table is
// one row per library folder, and the scan read needs an O(1) lookup per
// candidate, so it is read in full rather than per path (which would be an N+1
// on a poll that runs every ~700ms).
func (db *DB) ListSightings(ctx context.Context) (map[string]Sighting, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT source_path, first_seen_at, last_seen_at, acknowledged_at, baseline
		 FROM library_sightings`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Sighting{}
	for rows.Next() {
		var s Sighting
		var acked sql.NullString
		var baseline int
		if err := rows.Scan(&s.SourcePath, &s.FirstSeenAt, &s.LastSeenAt, &acked, &baseline); err != nil {
			return nil, err
		}
		s.AcknowledgedAt = acked.String
		s.Baseline = baseline != 0
		out[s.SourcePath] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AcknowledgeSightings marks paths as dismissed from the New view. Unknown paths
// are ignored: the client sends back what a scan handed it, and a folder that has
// since disappeared (or a stale tab) must not be an error - there is nothing to
// acknowledge and nothing to repair. An already-acknowledged path is re-stamped,
// which is harmless and keeps the call idempotent.
func (db *DB) AcknowledgeSightings(ctx context.Context, paths []string, at time.Time) error {
	if len(paths) == 0 {
		return nil
	}
	ts := sightingTime(at)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE library_sightings SET acknowledged_at=? WHERE source_path=?`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, ts, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HasSightings reports whether anything has ever been recorded. It answers the
// baseline question ("is this the first batch?") without reading the whole table.
func (db *DB) HasSightings(ctx context.Context) (bool, error) {
	var seen int
	err := db.sql.QueryRowContext(ctx, `SELECT 1 FROM library_sightings LIMIT 1`).Scan(&seen)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}
