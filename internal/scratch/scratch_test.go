package scratch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
)

func writeFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), 100)
	writeFile(t, filepath.Join(dir, "sub", "b.txt"), 250)
	got, err := DirSize(dir)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}
	if got != 350 {
		t.Errorf("DirSize = %d, want 350", got)
	}
	// A missing path is zero, not an error.
	if n, err := DirSize(filepath.Join(dir, "gone")); err != nil || n != 0 {
		t.Errorf("DirSize(missing) = %d,%v, want 0,nil", n, err)
	}
	if n, _ := DirSize(""); n != 0 {
		t.Errorf("DirSize(empty) = %d, want 0", n)
	}
}

func TestConfinedAllowedAndDenied(t *testing.T) {
	root := t.TempDir()
	// Allowed: a dir strictly inside the root.
	inside := filepath.Join(root, "book-abc")
	if _, ok := Confined(root, inside); !ok {
		t.Error("Confined denied a path inside the root")
	}
	// Denied: the root itself, a sibling outside, a traversal, and empties.
	denied := []struct {
		name          string
		root, workDir string
	}{
		{"root itself", root, root},
		{"outside sibling", root, t.TempDir()},
		{"traversal", root, filepath.Join(root, "..", "elsewhere")},
		{"empty root", "", inside},
		{"empty dir", root, ""},
	}
	for _, c := range denied {
		if _, ok := Confined(c.root, c.workDir); ok {
			t.Errorf("%s: Confined allowed a path it must reject", c.name)
		}
	}
}

func TestPurgeRemovesChaptersKeepsDurables(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "book-1")
	// chapters/ is reclaimable; probe.json/manifest.json/transcripts are durable.
	writeFile(t, filepath.Join(work, audio.ChaptersDir, "ch001.flac"), 1024)
	writeFile(t, filepath.Join(work, audio.ChaptersDir, "ch002.flac"), 1024)
	writeFile(t, filepath.Join(work, audio.ProbeName), 50)
	writeFile(t, filepath.Join(work, audio.ManifestName), 80)
	writeFile(t, filepath.Join(work, "transcripts-raw", "ch001.json"), 200)

	if err := Purge(root, work); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, audio.ChaptersDir)); !os.IsNotExist(err) {
		t.Error("Purge did not remove chapters/")
	}
	for _, keep := range []string{audio.ProbeName, audio.ManifestName, "transcripts-raw/ch001.json"} {
		if _, err := os.Stat(filepath.Join(work, keep)); err != nil {
			t.Errorf("Purge removed a durable it must keep: %s (%v)", keep, err)
		}
	}
}

// Purge must refuse (silently, as a no-op) a work dir outside the root - never
// deleting an arbitrary location.
func TestPurgeRefusesOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, audio.ChaptersDir, "ch001.flac"), 1024)
	if err := Purge(root, outside); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, audio.ChaptersDir)); err != nil {
		t.Error("Purge removed chapters/ outside the work root")
	}
}
