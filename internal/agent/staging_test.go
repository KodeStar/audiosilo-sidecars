package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStagingNewAndAccessors(t *testing.T) {
	work := t.TempDir()
	s, err := New(work, "fact_pass", 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wantDir := filepath.Join(work, "_runs", "fact_pass-a01")
	if s.Dir() != wantDir {
		t.Errorf("Dir = %q, want %q", s.Dir(), wantDir)
	}
	if s.OutDir() != filepath.Join(wantDir, "out") {
		t.Errorf("OutDir = %q", s.OutDir())
	}
	if fi, err := os.Stat(s.OutDir()); err != nil || !fi.IsDir() {
		t.Errorf("out dir not created: %v", err)
	}
}

func TestStagingCopyFileAndPerm(t *testing.T) {
	work := t.TempDir()
	s, _ := New(work, "s", 1)
	src := filepath.Join(t.TempDir(), "probe.json")
	if err := os.WriteFile(src, []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyFile(src, "probe.json"); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.Dir(), "probe.json"))
	if err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("copied content = %q err=%v", got, err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(s.Dir(), "probe.json"))
		if fi.Mode().Perm() != inputPerm {
			t.Errorf("perm = %o, want %o (read-only input)", fi.Mode().Perm(), inputPerm)
		}
	}
}

func TestStagingCopyFileRejectsTraversal(t *testing.T) {
	work := t.TempDir()
	s, _ := New(work, "s", 1)
	src := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	for _, rel := range []string{"../escape.txt", "/abs/path.txt", "..", "a/../../b"} {
		if err := s.CopyFile(src, rel); err == nil {
			t.Errorf("CopyFile(%q) allowed, want rejected", rel)
		}
	}
}

func TestStagingCopyDirWithFilter(t *testing.T) {
	work := t.TempDir()
	s, _ := New(work, "s", 1)
	srcDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(srcDir, "ch001.txt"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(srcDir, "ch002.txt"), []byte("b"), 0o644)
	_ = os.WriteFile(filepath.Join(srcDir, "skip.md"), []byte("c"), 0o644)
	err := s.CopyDir(srcDir, "transcripts-text", func(rel string) bool {
		return strings.HasSuffix(rel, ".txt")
	})
	if err != nil {
		t.Fatalf("CopyDir: %v", err)
	}
	base := filepath.Join(s.Dir(), "transcripts-text")
	if _, err := os.Stat(filepath.Join(base, "ch001.txt")); err != nil {
		t.Errorf("ch001.txt not copied")
	}
	if _, err := os.Stat(filepath.Join(base, "skip.md")); !os.IsNotExist(err) {
		t.Errorf("skip.md should have been filtered out")
	}
}

func TestStagingWriteFileRejectsTraversal(t *testing.T) {
	work := t.TempDir()
	s, _ := New(work, "s", 1)
	if err := s.WriteFile("marker.txt", []byte("ok")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := s.WriteFile("../evil.txt", []byte("no")); err == nil {
		t.Error("WriteFile traversal allowed")
	}
}

func TestHarvestHappyPath(t *testing.T) {
	work := t.TempDir()
	s, _ := New(work, "s", 1)
	if err := os.WriteFile(filepath.Join(s.OutDir(), "characters.json"), []byte(`{"work":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Harvest(s, []HarvestSpec{{From: "characters.json", To: "sidecars/characters.json"}})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(work, "sidecars", "characters.json"))
	if err != nil || string(got) != `{"work":"x"}` {
		t.Fatalf("harvested content = %q err=%v", got, err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(work, "sidecars", "characters.json"))
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("harvested perm = %o, want 644", fi.Mode().Perm())
		}
	}
}

func TestHarvestRejectsTraversal(t *testing.T) {
	work := t.TempDir()
	s, _ := New(work, "s", 1)
	_ = os.WriteFile(filepath.Join(s.OutDir(), "ok.json"), []byte("x"), 0o644)
	// From escaping out/.
	if err := Harvest(s, []HarvestSpec{{From: "../ok.json", To: "dst.json"}}); err == nil {
		t.Error("Harvest From-traversal allowed")
	}
	// To escaping the work dir.
	if err := Harvest(s, []HarvestSpec{{From: "ok.json", To: "../escape.json"}}); err == nil {
		t.Error("Harvest To-traversal allowed")
	}
	// Absolute To.
	if err := Harvest(s, []HarvestSpec{{From: "ok.json", To: "/etc/passwd"}}); err == nil {
		t.Error("Harvest absolute To allowed")
	}
}

func TestHarvestRejectsSymlinkInOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	work := t.TempDir()
	s, _ := New(work, "s", 1)
	// A direct symlink placed in out/ pointing outside.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(secret, []byte("SECRET"), 0o644)
	if err := os.Symlink(secret, filepath.Join(s.OutDir(), "link.json")); err != nil {
		t.Fatal(err)
	}
	if err := Harvest(s, []HarvestSpec{{From: "link.json", To: "dst.json"}}); err == nil {
		t.Error("Harvest of a symlink in out/ allowed")
	}
}

func TestHarvestRejectsIntermediateSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	work := t.TempDir()
	s, _ := New(work, "s", 1)
	// out/sub -> an outside directory holding a regular file.
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "file.txt"), []byte("SECRET"), 0o644)
	if err := os.Symlink(outside, filepath.Join(s.OutDir(), "sub")); err != nil {
		t.Fatal(err)
	}
	// The final component (file.txt) is a regular file, but a component symlink
	// redirects the resolved path outside out/ - EvalSymlinks containment must catch it.
	if err := Harvest(s, []HarvestSpec{{From: "sub/file.txt", To: "dst.txt"}}); err == nil {
		t.Error("Harvest through an escaping intermediate symlink allowed")
	}
}

func TestHarvestSizeCap(t *testing.T) {
	work := t.TempDir()
	s, _ := New(work, "s", 1)
	big := make([]byte, 4096)
	_ = os.WriteFile(filepath.Join(s.OutDir(), "big.bin"), big, 0o644)
	if err := Harvest(s, []HarvestSpec{{From: "big.bin", To: "dst.bin", MaxBytes: 1024}}); err == nil {
		t.Error("Harvest over the size cap allowed")
	}
	// Within a larger cap it is fine.
	if err := Harvest(s, []HarvestSpec{{From: "big.bin", To: "dst.bin", MaxBytes: 1 << 20}}); err != nil {
		t.Errorf("Harvest under cap failed: %v", err)
	}
}
