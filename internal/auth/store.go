package auth

import (
	"sync"
	"time"
)

// Store persists the admin password hash and the set of live session-token
// hashes. It is deliberately storage-agnostic: tests use MemStore, and the
// daemon uses the SQLite-backed adapter from internal/store (store.AuthStore),
// which satisfies this interface structurally. Session tokens are stored ONLY as
// SHA-256 hashes, never in plaintext.
//
// M0 shipped a JSON file store here; M1 migrated durable auth into the database
// (one state file), so the file store was removed rather than left as dead code.
type Store interface {
	// LoadAuth returns the stored password hash, or "" if no admin exists yet.
	LoadAuth() (passwordHash string, err error)
	// SaveAuth stores the password hash (argon2id encoded).
	SaveAuth(passwordHash string) error
	// AddSession records a live session by its token hash.
	AddSession(tokenHash string, createdAt time.Time) error
	// HasSession reports whether a session with tokenHash exists.
	HasSession(tokenHash string) (bool, error)
	// RemoveSession deletes a session by its token hash (no-op if absent).
	RemoveSession(tokenHash string) error
}

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu       sync.Mutex
	passHash string
	sessions map[string]time.Time
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{sessions: map[string]time.Time{}}
}

// LoadAuth returns the stored password hash.
func (m *MemStore) LoadAuth() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.passHash, nil
}

// SaveAuth stores the password hash.
func (m *MemStore) SaveAuth(passwordHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.passHash = passwordHash
	return nil
}

// AddSession records a session token hash.
func (m *MemStore) AddSession(tokenHash string, createdAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[tokenHash] = createdAt
	return nil
}

// HasSession reports whether the session token hash is present.
func (m *MemStore) HasSession(tokenHash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[tokenHash]
	return ok, nil
}

// RemoveSession removes a session token hash.
func (m *MemStore) RemoveSession(tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tokenHash)
	return nil
}
