package store

import (
	"context"
)

// Override is a persistent, path-keyed candidate override: a manual hide and/or a
// manual work match for a scanned book. It is durable config decoupled from the
// rebuildable book index (keyed by source_path, no FK), so it applies to every
// future scan and survives a restart.
type Override struct {
	SourcePath string
	Hidden     bool
	WorkID     string
	WorkTitle  string
	UpdatedAt  string
}

// meaningful reports whether an override says anything worth persisting. A row
// that is neither hidden nor manually matched is the absence of an override, so
// UpsertOverride deletes it rather than storing an inert row.
func (o Override) meaningful() bool { return o.Hidden || o.WorkID != "" }

// UpsertOverride writes the full desired state for a source_path and returns the
// stored row (so callers need no follow-up GetOverride for updated_at). It
// implements delete-on-empty: an override that is neither hidden nor manually
// matched removes the row (a no-op when absent) and returns the cleared input
// unchanged, so clearing both toggles leaves no trace. Any other state upserts
// the row and stamps updated_at, which the returned row carries.
func (db *DB) UpsertOverride(ctx context.Context, ov Override) (Override, error) {
	if !ov.meaningful() {
		_, err := db.sql.ExecContext(ctx,
			`DELETE FROM candidate_overrides WHERE source_path=?`, ov.SourcePath)
		return ov, err
	}
	ts := timestamp(nowFn())
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO candidate_overrides (source_path, hidden, work_id, work_title, updated_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(source_path) DO UPDATE SET
		   hidden=excluded.hidden, work_id=excluded.work_id,
		   work_title=excluded.work_title, updated_at=excluded.updated_at`,
		ov.SourcePath, boolToInt(ov.Hidden), ov.WorkID, ov.WorkTitle, ts)
	if err != nil {
		return Override{}, err
	}
	ov.UpdatedAt = ts
	return ov, nil
}

// ListOverrides returns all overrides ordered by source_path.
func (db *DB) ListOverrides(ctx context.Context) ([]Override, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT source_path, hidden, work_id, work_title, updated_at
		 FROM candidate_overrides ORDER BY source_path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Override
	for rows.Next() {
		ov, err := scanOverride(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ov)
	}
	return out, rows.Err()
}

func scanOverride(sc interface{ Scan(...any) error }) (Override, error) {
	var ov Override
	var hidden int
	if err := sc.Scan(&ov.SourcePath, &hidden, &ov.WorkID, &ov.WorkTitle, &ov.UpdatedAt); err != nil {
		return Override{}, err
	}
	ov.Hidden = hidden != 0
	return ov, nil
}
