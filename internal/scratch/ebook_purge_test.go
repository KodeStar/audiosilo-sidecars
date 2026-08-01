package scratch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/ebook"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

// TestPurgeKindAware pins what each kind reclaims, and - more importantly - what it
// must NOT.
//
// An audio book's transcripts-corrected/ is DURABLE: it is the n-gram source for a
// later re-validation and the corpus a sequel's spelling carryover copies from.
// An ebook's text is the book's copyrighted source prose, which must not outlive
// the derivation. One shared list could not satisfy both.
func TestPurgeKindAware(t *testing.T) {
	seed := func(t *testing.T) string {
		t.Helper()
		wd := t.TempDir()
		for _, d := range []string{
			"chapters", "_runs", ebook.ExtractDir, ebook.TextDir,
			spelling.CorrectedDir, "facts", "sidecars",
		} {
			if err := os.MkdirAll(filepath.Join(wd, d), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wd, d, "f.txt"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return wd
	}
	exists := func(wd, d string) bool {
		_, err := os.Stat(filepath.Join(wd, d))
		return err == nil
	}

	t.Run("audio keeps its corrected transcripts", func(t *testing.T) {
		wd := seed(t)
		if err := Purge(filepath.Dir(wd), wd, state.KindAudio); err != nil {
			t.Fatal(err)
		}
		if exists(wd, "chapters") || exists(wd, "_runs") {
			t.Error("audio purge left the split/agent scratch behind")
		}
		if !exists(wd, spelling.CorrectedDir) {
			t.Error("audio purge deleted transcripts-corrected/, which is durable: " +
				"it is the n-gram source and a sequel's spelling-carryover corpus")
		}
		if !exists(wd, "facts") || !exists(wd, "sidecars") {
			t.Error("audio purge deleted a durable")
		}
	})

	t.Run("ebook also reclaims its source prose", func(t *testing.T) {
		wd := seed(t)
		if err := Purge(filepath.Dir(wd), wd, state.KindEbook); err != nil {
			t.Fatal(err)
		}
		if exists(wd, ebook.ExtractDir) || exists(wd, ebook.TextDir) {
			t.Error("ebook purge left the book's source text on disk; it must not outlive the derivation")
		}
		if !exists(wd, "facts") || !exists(wd, "sidecars") {
			t.Error("ebook purge deleted a durable")
		}
	})

	t.Run("ebook also strips the prose kept in the durable manifest", func(t *testing.T) {
		wd := seed(t)
		if err := ebook.WriteManifest(wd, ebook.Universe{Docs: []ebook.Doc{
			{Index: 1, File: "001.txt", Label: "Chapter 1", Head: "It was a bright cold day in April"},
			{Index: 2, File: "002.txt", Label: "Chapter 2", Head: "The hallway smelt of boiled cabbage"},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := Purge(filepath.Dir(wd), wd, state.KindEbook); err != nil {
			t.Fatal(err)
		}
		// The manifest itself is a durable: a re-entered chapter_mapping reads it.
		u, err := ebook.ReadManifest(wd)
		if err != nil {
			t.Fatalf("ebook purge removed the extract manifest, which chapter_mapping needs: %v", err)
		}
		if len(u.Docs) != 2 {
			t.Fatalf("docs = %d, want the 2 recorded sections", len(u.Docs))
		}
		for _, d := range u.Docs {
			if d.Head != "" {
				t.Errorf("section %d kept its opening words (%q) through the purge; "+
					"40 words per section is the author's prose outliving the derivation", d.Index, d.Head)
			}
			if d.Label == "" {
				t.Errorf("section %d lost its label; only Head is prose", d.Index)
			}
		}
	})

	t.Run("an unknown or empty kind behaves as audio", func(t *testing.T) {
		wd := seed(t)
		if err := Purge(filepath.Dir(wd), wd, ""); err != nil {
			t.Fatal(err)
		}
		if !exists(wd, spelling.CorrectedDir) {
			t.Error("a pre-migration book must reclaim exactly what it always did")
		}
	})

	t.Run("a work dir outside the root is untouched", func(t *testing.T) {
		wd := seed(t)
		if err := Purge(t.TempDir(), wd, state.KindEbook); err != nil {
			t.Fatal(err)
		}
		if !exists(wd, ebook.TextDir) {
			t.Error("purge escaped its confinement root")
		}
	})
}
