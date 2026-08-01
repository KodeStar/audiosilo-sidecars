package scratch

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
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
	// chapters/, _runs/, clips/, retranscribe/ are reclaimable; probe.json/
	// manifest.json/transcripts/facts are durable.
	writeFile(t, filepath.Join(work, audio.ChaptersDir, "ch001.flac"), 1024)
	writeFile(t, filepath.Join(work, audio.ChaptersDir, "ch002.flac"), 1024)
	writeFile(t, filepath.Join(work, "_runs", "fact_pass-a01", "out", "x.md"), 300)
	writeFile(t, filepath.Join(work, "clips", "t002.flac"), 400)
	writeFile(t, filepath.Join(work, "retranscribe", "ch002.json"), 500)
	writeFile(t, filepath.Join(work, audio.ProbeName), 50)
	writeFile(t, filepath.Join(work, audio.ManifestName), 80)
	writeFile(t, filepath.Join(work, "transcripts-raw", "ch001.json"), 200)

	if err := Purge(root, work, state.KindAudio); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	for _, gone := range []string{audio.ChaptersDir, "_runs", "clips", "retranscribe"} {
		if _, err := os.Stat(filepath.Join(work, gone)); !os.IsNotExist(err) {
			t.Errorf("Purge did not remove reclaimable %s/", gone)
		}
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
	if err := Purge(root, outside, state.KindAudio); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, audio.ChaptersDir)); err != nil {
		t.Error("Purge removed chapters/ outside the work root")
	}
}

// TestInvalidatedStagesCoversEveryReclaimedDir is the guard on the coupling the
// artifact table exists to hold. A reclaimed directory whose producing stage keeps
// its sentinel wedges the book permanently: Retry re-runs an earlier stage, the
// skipped stage never rebuilds the directory, and its successor fails forever on the
// missing input. Retry only drops the CURRENT stage's sentinel, so there is no way
// out but delete and re-enqueue.
func TestInvalidatedStagesCoversEveryReclaimedDir(t *testing.T) {
	for _, kind := range []state.Kind{state.KindAudio, state.KindEbook} {
		stages := InvalidatedStages(kind)
		for _, a := range artifactsFor(kind) {
			if a.Stage == "" {
				continue
			}
			if !slices.Contains(stages, a.Stage) {
				t.Errorf("kind %s reclaims %s but does not invalidate %s", kind, a.Dir, a.Stage)
			}
		}
	}
}

// TestEbookTextLayerInvalidatesBothWriters: ebook-text/ is written by extracting when
// the derived universe is contiguous and by chapter_mapping when it is not, so a
// purge must re-run whichever produced it. Listing only extracting wedged every book
// that needed the mapping agent - which, on the validation corpus, is a third of them.
func TestEbookTextLayerInvalidatesBothWriters(t *testing.T) {
	stages := InvalidatedStages(state.KindEbook)
	for _, want := range []state.State{state.Extracting, state.ChapterMapping} {
		if !slices.Contains(stages, want) {
			t.Errorf("ebook purge does not invalidate %s; a Retry would skip it into an empty text layer", want)
		}
	}
	// An audio book must be unaffected by the ebook entries.
	if got := InvalidatedStages(state.KindAudio); !slices.Equal(got, []state.State{state.Splitting}) {
		t.Errorf("audio invalidates %v, want only splitting", got)
	}
}

// TestPurgeRequiredIsEbookOnly pins the licensing predicate: an ebook's reclaimed
// layers are its copyrighted source prose, so the purge is an obligation; audio
// scratch is a disk-space preference the operator may switch off.
func TestPurgeRequiredIsEbookOnly(t *testing.T) {
	if !PurgeRequired(state.KindEbook) {
		t.Error("an ebook purge must not be optional")
	}
	if PurgeRequired(state.KindAudio) || PurgeRequired("") {
		t.Error("audio scratch is a preference, not an obligation")
	}
}
