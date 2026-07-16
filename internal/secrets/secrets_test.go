package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	// Absent -> "" and not present.
	if v, _ := fs.Get(AnthropicAPIKey); v != "" {
		t.Errorf("absent Get = %q, want empty", v)
	}
	if p, _ := fs.Present(AnthropicAPIKey); p {
		t.Error("absent Present = true")
	}

	// Set -> present, value stored.
	if err := fs.Set(AnthropicAPIKey, "sk-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if p, _ := fs.Present(AnthropicAPIKey); !p {
		t.Error("after Set, Present = false")
	}
	if v, _ := fs.Get(AnthropicAPIKey); v != "sk-secret" {
		t.Errorf("Get = %q", v)
	}

	// Delete -> absent.
	if err := fs.Delete(AnthropicAPIKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if p, _ := fs.Present(AnthropicAPIKey); p {
		t.Error("after Delete, Present = true")
	}
}

func TestFileStorePersistsAndMode0600(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Set(GitHubPAT, "ghp_xyz"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// File must be 0600 (it holds plaintext secrets).
	info, err := os.Stat(filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets.json perm = %o, want 600", perm)
	}

	// Reopen: value survives.
	fs2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if v, _ := fs2.Get(GitHubPAT); v != "ghp_xyz" {
		t.Errorf("after reopen Get = %q", v)
	}
}

func TestNamesStable(t *testing.T) {
	got := Names()
	want := []string{AnthropicAPIKey, OpenAIAPIKey, GitHubPAT}
	if len(got) != len(want) {
		t.Fatalf("Names len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
