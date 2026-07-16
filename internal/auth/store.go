package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store persists the admin password hash and the set of live session-token
// hashes. It is deliberately storage-agnostic: M0 ships a JSON file store and an
// in-memory store; M1 can add a SQLite implementation without touching the auth
// service. Session tokens are stored ONLY as SHA-256 hashes, never in plaintext.
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

// MemStore is an in-memory Store for tests and as the runtime cache. It is not
// persisted on its own.
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

// authFile is the on-disk shape of auth.json.
type authFile struct {
	PasswordHash string `json:"password_hash"`
}

// sessionsFile is the on-disk shape of sessions.json. Only token hashes are
// stored (never the raw tokens).
type sessionsFile struct {
	Sessions map[string]time.Time `json:"sessions"`
}

// FileStore persists auth.json and sessions.json inside a data directory, both
// with 0600 permissions. It keeps an in-memory copy and writes through on every
// mutation. Concurrency is serialized by a single mutex - the session set is tiny
// and writes are infrequent, so this is more than adequate for M0.
type FileStore struct {
	mu       sync.Mutex
	dir      string
	passHash string
	sessions map[string]time.Time
}

// NewFileStore opens (or initializes) a file store in dir, loading any existing
// auth.json / sessions.json.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fs := &FileStore{dir: dir, sessions: map[string]time.Time{}}
	if err := fs.load(); err != nil {
		return nil, err
	}
	return fs, nil
}

func (f *FileStore) authPath() string     { return filepath.Join(f.dir, "auth.json") }
func (f *FileStore) sessionsPath() string { return filepath.Join(f.dir, "sessions.json") }

func (f *FileStore) load() error {
	if raw, err := os.ReadFile(f.authPath()); err == nil {
		var a authFile
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		f.passHash = a.PasswordHash
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if raw, err := os.ReadFile(f.sessionsPath()); err == nil {
		var s sessionsFile
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if s.Sessions != nil {
			f.sessions = s.Sessions
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// writeJSON atomically writes v as JSON to path with 0600 permissions.
func writeJSON(path string, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadAuth returns the stored password hash.
func (f *FileStore) LoadAuth() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.passHash, nil
}

// SaveAuth persists the password hash to auth.json.
func (f *FileStore) SaveAuth(passwordHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.passHash = passwordHash
	return writeJSON(f.authPath(), authFile{PasswordHash: passwordHash})
}

// AddSession records a session and persists sessions.json.
func (f *FileStore) AddSession(tokenHash string, createdAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[tokenHash] = createdAt
	return f.persistSessions()
}

// HasSession reports whether tokenHash is a live session.
func (f *FileStore) HasSession(tokenHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.sessions[tokenHash]
	return ok, nil
}

// RemoveSession deletes a session and persists sessions.json.
func (f *FileStore) RemoveSession(tokenHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, tokenHash)
	return f.persistSessions()
}

func (f *FileStore) persistSessions() error {
	return writeJSON(f.sessionsPath(), sessionsFile{Sessions: f.sessions})
}
