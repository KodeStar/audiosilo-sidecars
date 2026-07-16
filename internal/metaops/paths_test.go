package metaops

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathAllowedEmptyRootsAllowsAny(t *testing.T) {
	ok, err := PathAllowed("/anywhere/at/all", nil)
	if err != nil || !ok {
		t.Fatalf("empty roots should allow any path: ok=%v err=%v", ok, err)
	}
}

func TestPathAllowedInsideAndOutsideRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "series", "book")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()

	// Allowed: inside the root.
	if ok, err := PathAllowed(inside, []string{root}); err != nil || !ok {
		t.Errorf("inside root should be allowed: ok=%v err=%v", ok, err)
	}
	// Allowed: the root itself.
	if ok, _ := PathAllowed(root, []string{root}); !ok {
		t.Error("the root itself should be allowed")
	}
	// Denied: a sibling directory outside every root.
	if ok, _ := PathAllowed(outside, []string{root}); ok {
		t.Error("a path outside all roots should be denied")
	}
	// Denied: a prefix-sibling that is not actually nested (root + suffix).
	if ok, _ := PathAllowed(root+"-evil", []string{root}); ok {
		t.Error("a string-prefix sibling should not be treated as nested")
	}
}

func TestPathAllowedSymlinkEscapeDenied(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the root pointing outside it must not smuggle access.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if ok, _ := PathAllowed(link, []string{root}); ok {
		t.Error("a symlink escaping the root should be denied")
	}
}
