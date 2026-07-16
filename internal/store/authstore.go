package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// authPasswordKey is the settings key holding the admin argon2id password hash.
// Keeping it in the settings KV (rather than a separate auth.json) means the
// daemon has a single durable state file - the DB - which the M1 migration off
// the JSON files establishes.
const authPasswordKey = "auth.password_hash"

// AuthStore adapts *DB to the auth.Store interface (LoadAuth/SaveAuth +
// session CRUD) so the auth Manager persists to SQLite. It is defined here
// (rather than in internal/auth) to avoid a store->auth import; internal/auth
// depends only on its own Store interface, which this satisfies structurally.
type AuthStore struct {
	db  *DB
	ctx context.Context //nolint:containedctx // background ctx for a long-lived adapter
}

// AuthStore returns an auth.Store-compatible adapter over the database. The
// adapter carries a background context because the auth.Store interface predates
// context threading; store operations here are trivial local SQLite writes.
func (db *DB) AuthStore() *AuthStore {
	return &AuthStore{db: db, ctx: context.Background()}
}

// LoadAuth returns the stored admin password hash, or "" when unset.
func (a *AuthStore) LoadAuth() (string, error) {
	v, _, err := a.db.GetSetting(a.ctx, authPasswordKey)
	return v, err
}

// SaveAuth stores the admin password hash.
func (a *AuthStore) SaveAuth(passwordHash string) error {
	return a.db.SetSetting(a.ctx, authPasswordKey, passwordHash)
}

// AddSession records a live session by its token hash.
func (a *AuthStore) AddSession(tokenHash string, createdAt time.Time) error {
	_, err := a.db.sql.ExecContext(a.ctx,
		`INSERT INTO sessions (token_hash, created_at) VALUES (?,?)
		 ON CONFLICT(token_hash) DO NOTHING`, tokenHash, timestamp(createdAt))
	return err
}

// HasSession reports whether tokenHash names a live session.
func (a *AuthStore) HasSession(tokenHash string) (bool, error) {
	var one int
	err := a.db.sql.QueryRowContext(a.ctx,
		`SELECT 1 FROM sessions WHERE token_hash=?`, tokenHash).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RemoveSession deletes a session by token hash (no-op if absent).
func (a *AuthStore) RemoveSession(tokenHash string) error {
	_, err := a.db.sql.ExecContext(a.ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}
