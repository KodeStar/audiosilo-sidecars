// Package auth manages the single admin password and opaque session tokens for
// the audiosilo-sidecars daemon. The first-run password is generated once and
// printed once in the startup banner (never stored or logged in plaintext);
// session tokens are stored only as SHA-256 hashes. The storage backend is
// pluggable (Store) so M1 can swap the JSON file store for SQLite.
package auth

import (
	"errors"
	"time"
)

// MinPasswordLen is the minimum length for a user-chosen password.
const MinPasswordLen = 8

// Errors returned by the Manager.
var (
	// ErrInvalidCreds is returned for a wrong password (login or change).
	ErrInvalidCreds = errors.New("invalid credentials")
	// ErrPasswordTooShort is returned when a new password is below MinPasswordLen.
	ErrPasswordTooShort = errors.New("password too short")
	// ErrNoAdmin is returned by Login when no admin has been provisioned yet.
	ErrNoAdmin = errors.New("no admin configured")
)

// Manager is the auth service over a Store.
type Manager struct {
	store Store
	now   func() time.Time
}

// New returns a Manager backed by store.
func New(store Store) *Manager {
	return &Manager{store: store, now: time.Now}
}

// EnsureAdmin provisions the admin on first run. If no password hash exists it
// generates a one-time password, stores its argon2id hash, and returns the
// plaintext so the caller can print it ONCE in the startup banner. If an admin
// already exists it returns ("", nil) - the password is never re-printed.
func (m *Manager) EnsureAdmin() (oneTimePassword string, err error) {
	hash, err := m.store.LoadAuth()
	if err != nil {
		return "", err
	}
	if hash != "" {
		return "", nil
	}
	pw, err := generatePassword()
	if err != nil {
		return "", err
	}
	enc, err := HashPassword(pw)
	if err != nil {
		return "", err
	}
	if err := m.store.SaveAuth(enc); err != nil {
		return "", err
	}
	return pw, nil
}

// Login verifies password and, on success, mints and stores a new session token,
// returning the raw secret (the only time it exists outside the client).
func (m *Manager) Login(password string) (string, error) {
	hash, err := m.store.LoadAuth()
	if err != nil {
		return "", err
	}
	if hash == "" {
		return "", ErrNoAdmin
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrInvalidCreds
	}
	return m.mintSession()
}

// mintSession creates a session and returns the raw token secret.
func (m *Manager) mintSession() (string, error) {
	secret, tokenHash, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := m.store.AddSession(tokenHash, m.now()); err != nil {
		return "", err
	}
	return secret, nil
}

// Resolve reports whether token is a live session.
func (m *Manager) Resolve(token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	return m.store.HasSession(hashSecret(token))
}

// Logout revokes the session identified by token (no-op if unknown).
func (m *Manager) Logout(token string) error {
	if token == "" {
		return nil
	}
	return m.store.RemoveSession(hashSecret(token))
}

// ChangePassword verifies current and sets a new password hash. It enforces the
// minimum length on the new password. The existing sessions are intentionally
// left intact (the caller stays signed in).
func (m *Manager) ChangePassword(current, newPassword string) error {
	hash, err := m.store.LoadAuth()
	if err != nil {
		return err
	}
	if hash == "" {
		return ErrNoAdmin
	}
	ok, err := VerifyPassword(current, hash)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCreds
	}
	if len(newPassword) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	enc, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return m.store.SaveAuth(enc)
}
