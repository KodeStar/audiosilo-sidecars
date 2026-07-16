package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileAtomicCreatesParentAndWrites(t *testing.T) {
	dir := t.TempDir()
	// A path two levels deep to prove the parent tree is created.
	path := filepath.Join(dir, "sub", "nested", "file.json")
	data := []byte("hello\n")
	if err := WriteFileAtomic(path, data, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content = %q, want %q", got, data)
	}
	// No stray temp file left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want 1 (no temp leftover)", len(entries))
	}
}

func TestWriteFileAtomicAppliesPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := WriteFileAtomic(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestWriteFileAtomicOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := WriteFileAtomic(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want %q", got, "second")
	}
}

func TestIsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsFile(file) {
		t.Error("IsFile(regular file) = false, want true")
	}
	if IsFile(dir) {
		t.Error("IsFile(dir) = true, want false")
	}
	if IsFile(filepath.Join(dir, "missing")) {
		t.Error("IsFile(missing) = true, want false")
	}
}
