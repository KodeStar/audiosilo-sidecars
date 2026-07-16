// Package secrets stores sensitive values (LLM API keys, a GitHub PAT) OUTSIDE
// the config file. The OS keychain is the preferred backend; when no keychain is
// available (headless Linux without a Secret Service) it falls back to a 0600
// secrets.json inside the data directory. Values are write-only from the API's
// point of view: callers can Set/Delete and probe Present, but the read API only
// ever exposes presence booleans, never the secret itself.
package secrets

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/zalando/go-keyring"
)

// keyringService is the service name under which secrets are filed in the OS
// keychain (macOS Keychain / Windows Credential Manager / libsecret on Linux).
const keyringService = "audiosilo-sidecars"

// Named secrets recognized by the daemon.
const (
	AnthropicAPIKey = "anthropic_api_key"
	OpenAIAPIKey    = "openai_api_key"
	GitHubPAT       = "github_pat"
)

// Names returns the recognized secret names in a stable order (drives the
// Settings presence enumeration).
func Names() []string {
	return []string{AnthropicAPIKey, OpenAIAPIKey, GitHubPAT}
}

// Store is a named-secret backend. Get returns ("", nil) for an absent name so
// callers can treat "no secret yet" as a normal state.
type Store interface {
	Get(name string) (string, error)
	Set(name, value string) error
	Delete(name string) error
	Present(name string) (bool, error)
}

// Open returns the best available secrets store for dir. It probes the OS
// keychain with a sentinel round-trip; if the keychain is unavailable it returns
// a 0600 file store and usingFallback=true so the caller can warn the operator.
func Open(dir string) (store Store, usingFallback bool, err error) {
	if keyringUsable() {
		return Keyring{}, false, nil
	}
	fs, err := NewFileStore(dir)
	if err != nil {
		return nil, false, err
	}
	return fs, true, nil
}

// keyringUsable reports whether the OS keychain accepts a sentinel write.
func keyringUsable() bool {
	const probe = "__probe__"
	if err := keyring.Set(keyringService, probe, "1"); err != nil {
		return false
	}
	_ = keyring.Delete(keyringService, probe)
	return true
}

// Keyring is the OS-keychain-backed Store.
type Keyring struct{}

// Set stores value under name.
func (Keyring) Set(name, value string) error { return keyring.Set(keyringService, name, value) }

// Get returns the value for name, or ("", nil) when absent.
func (Keyring) Get(name string) (string, error) {
	v, err := keyring.Get(keyringService, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	return v, err
}

// Delete removes name; deleting an absent name is a no-op.
func (Keyring) Delete(name string) error {
	err := keyring.Delete(keyringService, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// Present reports whether name has a non-empty value.
func (k Keyring) Present(name string) (bool, error) {
	v, err := k.Get(name)
	return v != "", err
}

// FileStore is the 0600-file fallback Store. secrets.json holds a flat name ->
// value map. It is only used when no OS keychain backend exists.
type FileStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

// NewFileStore opens (or initializes) a file-backed secrets store in dir.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fs := &FileStore{path: filepath.Join(dir, "secrets.json"), m: map[string]string{}}
	raw, err := os.ReadFile(fs.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &fs.m); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		// New store.
	default:
		return nil, err
	}
	return fs, nil
}

// Get returns the value for name, or ("", nil) when absent.
func (f *FileStore) Get(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[name], nil
}

// Set stores value under name and persists the file.
func (f *FileStore) Set(name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[name] = value
	return f.persist()
}

// Delete removes name and persists the file.
func (f *FileStore) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, name)
	return f.persist()
}

// Present reports whether name has a non-empty value.
func (f *FileStore) Present(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[name] != "", nil
}

// persist writes secrets.json atomically with 0600 permissions. It sorts keys
// for a stable file. Caller holds the mutex.
func (f *FileStore) persist() error {
	keys := make([]string, 0, len(f.m))
	for k := range f.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(f.m))
	for _, k := range keys {
		ordered[k] = f.m[k]
	}
	out, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu sync.Mutex
	m  map[string]string
}

// NewMemStore returns an empty in-memory secrets store.
func NewMemStore() *MemStore { return &MemStore{m: map[string]string{}} }

// Get returns the value for name, or ("", nil) when absent.
func (s *MemStore) Get(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[name], nil
}

// Set stores value under name.
func (s *MemStore) Set(name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = value
	return nil
}

// Delete removes name.
func (s *MemStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
	return nil
}

// Present reports whether name has a non-empty value.
func (s *MemStore) Present(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[name] != "", nil
}
