package metaops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFixture creates a tiny nested audiobook tree: two series, each with a
// single-file book folder holding a dummy .m4b (enough for path-heuristic
// scanning with ffprobe disabled).
func writeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dirs := []string{
		filepath.Join(root, "Alex Maher", "The Hedge Wizard", "01 - The Hedge Wizard"),
		filepath.Join(root, "Alex Maher", "The Hedge Wizard", "02 - The Hedge Wizard 2"),
		filepath.Join(root, "Jane Doe", "Other Series", "01 - Book One"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "audio.m4b"), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func waitDone(t *testing.T, m *ScanManager, id string) ScanJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := m.Get(id)
		if !ok {
			t.Fatal("job vanished")
		}
		if job.Status != ScanRunning {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scan did not finish in time")
	return ScanJob{}
}

func TestScanManagerFindsBooksWithDisabledCoverage(t *testing.T) {
	root := writeFixture(t)
	// Disabled coverage client (no base URL) + ffprobe disabled.
	m := NewScanManager(context.Background(), NewClient(""), "")

	id, err := m.Start(root)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)
	if job.Status != ScanDone {
		t.Fatalf("status = %q (err %q)", job.Status, job.Error)
	}
	if job.Result == nil || len(job.Result.Books) != 3 {
		t.Fatalf("expected 3 books, got %+v", job.Result)
	}
	if job.Progress.Phase != "done" || job.Progress.Done != 3 || job.Progress.Total != 3 {
		t.Fatalf("progress = %+v", job.Progress)
	}
	for _, b := range job.Result.Books {
		if b.Coverage.Available {
			t.Errorf("book %q coverage should be unavailable (disabled client): %+v", b.Title, b.Coverage)
		}
		if b.Title == "" || b.AudioFiles == 0 {
			t.Errorf("book missing basic fields: %+v", b)
		}
	}
}

func TestScanManagerRejectsBadPath(t *testing.T) {
	m := NewScanManager(context.Background(), NewClient(""), "")
	if _, err := m.Start(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("Start on a missing path should error")
	}
	// A file, not a directory.
	f := filepath.Join(t.TempDir(), "file.txt")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if _, err := m.Start(f); err == nil {
		t.Error("Start on a file should error")
	}
	if _, ok := m.Get("nonexistent"); ok {
		t.Error("Get of unknown id should be false")
	}
}
