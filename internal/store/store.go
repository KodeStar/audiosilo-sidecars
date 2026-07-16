// Package store is the SQLite persistence layer for the sidecars daemon. It holds
// the scheduling truth: books and their pipeline state, the per-execution
// stage_runs ledger, live progress counters, a durable event log, and the auth
// password/session state (migrated off the M0 JSON files). The work-dir artifacts
// and _done/<stage>.json sentinels remain the content truth; the scheduler
// reconciles the two on startup.
//
// It uses modernc.org/sqlite (pure Go, no CGO) so the binary cross-compiles
// without a C toolchain - the workspace standard. Writers serialize on a single
// connection with WAL journaling, mirroring audiosilo-server. Store methods are
// plain, tested CRUD; business logic lives in internal/scheduler and
// internal/state.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// FileName is the database filename inside the data directory.
const FileName = "sidecars.db"

// DB is the sidecars database handle. A single writer connection serializes
// writes (SQLite's model), which is ample for a single-user local daemon and
// avoids "database is locked" churn.
type DB struct {
	sql *sql.DB
}

// nowFn is overridable in tests.
var nowFn = func() time.Time { return time.Now().UTC() }

// timeLayout is a FIXED-WIDTH UTC layout: nine fractional digits and a literal
// Z, so a lexicographic string compare of two timestamps is always chronological.
// time.RFC3339Nano trims trailing zeros in the fraction, which breaks that
// invariant ("...00Z" sorts after "...00.5Z" though it is earlier); the web
// sortBooks helper relies on the ordering holding.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// timestamp formats a moment in the store's canonical fixed-width UTC form.
func timestamp(t time.Time) string { return t.UTC().Format(timeLayout) }

// Open opens (creating if needed) the SQLite database at dsn and applies pending
// migrations. Pass ":memory:" for tests. WAL + busy_timeout + foreign_keys are
// applied via DSN pragmas; foreign_keys is load-bearing for the ON DELETE CASCADE
// rules and defaults OFF per connection, so it must always be set.
func Open(ctx context.Context, dsn string) (*DB, error) {
	full := dsn + pragmaSep(dsn) +
		"_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	sqldb, err := sql.Open("sqlite", full)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection: writers serialize, and an in-memory DSN is per-connection
	// so a pool would open distinct empty databases.
	sqldb.SetMaxOpenConns(1)
	db := &DB{sql: sqldb}
	if err := db.migrate(ctx); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	return db, nil
}

// pragmaSep returns the right query-string separator for appending pragmas.
func pragmaSep(dsn string) string {
	if strings.Contains(dsn, "?") {
		return "&"
	}
	return "?"
}

// Close closes the database.
func (db *DB) Close() error { return db.sql.Close() }

// migrate applies embedded migrations in filename order, recording each in
// schema_migrations so they run exactly once.
func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var seen int
		err := db.sql.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE name = ?`, name).Scan(&seen)
		if err == nil {
			continue // already applied
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		body, rerr := migrationsFS.ReadFile("migrations/" + name)
		if rerr != nil {
			return rerr
		}
		tx, berr := db.sql.BeginTx(ctx, nil)
		if berr != nil {
			return berr
		}
		if _, eerr := tx.ExecContext(ctx, string(body)); eerr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, eerr)
		}
		if _, eerr := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?)`,
			name, timestamp(nowFn())); eerr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, eerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return fmt.Errorf("commit migration %s: %w", name, cerr)
		}
	}
	return nil
}
