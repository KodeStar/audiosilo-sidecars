package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// newSeriesBook creates a book in db with a work dir under root and, when withFinal is
// set, seeds facts/knowledge-final.md so it qualifies as a carryover predecessor.
func newSeriesBook(t *testing.T, db *store.DB, root, series, pos string, withFinal bool) store.Book {
	t.Helper()
	work := filepath.Join(root, "work-"+series+"-"+pos)
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	if withFinal {
		if err := os.MkdirAll(filepath.Join(work, spelling.FactsDir), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, spelling.FactsDir, knowledgeFinalName), []byte("ENDING\n"), 0o644); err != nil { //nolint:gosec // test artifact
			t.Fatal(err)
		}
	}
	b, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: filepath.Join(root, series+"-"+pos+".m4b"),
		WorkDir:    work,
		Title:      series + " " + pos,
		Series:     series,
		SeriesPos:  pos,
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	return b
}

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestFindSeriesPredecessorNone(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()

	// A seriesless book never has a predecessor.
	loner := newSeriesBook(t, db, root, "", "1", true)
	if _, ok, err := findSeriesPredecessor(context.Background(), db, loner); err != nil || ok {
		t.Errorf("seriesless book: got ok=%v err=%v, want false/nil", ok, err)
	}

	// The first volume in a series has no earlier book.
	first := newSeriesBook(t, db, root, "Mistborn", "1", true)
	if _, ok, err := findSeriesPredecessor(context.Background(), db, first); err != nil || ok {
		t.Errorf("first volume: got ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestFindSeriesPredecessorFound(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	_ = newSeriesBook(t, db, root, "Stormlight", "1", true)
	two := newSeriesBook(t, db, root, "Stormlight", "2", true)
	three := newSeriesBook(t, db, root, "Stormlight", "3", false)

	pred, ok, err := findSeriesPredecessor(context.Background(), db, three)
	if err != nil || !ok {
		t.Fatalf("got ok=%v err=%v, want a predecessor", ok, err)
	}
	if pred.ID != two.ID {
		t.Errorf("predecessor id = %d, want book 2 (id %d) - the highest position below 3", pred.ID, two.ID)
	}
}

func TestFindSeriesPredecessorSkipsWithoutKnowledgeFinal(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	one := newSeriesBook(t, db, root, "Reckoners", "1", true) // qualifies
	_ = newSeriesBook(t, db, root, "Reckoners", "2", false)   // no knowledge-final -> skipped
	three := newSeriesBook(t, db, root, "Reckoners", "3", false)

	pred, ok, err := findSeriesPredecessor(context.Background(), db, three)
	if err != nil || !ok {
		t.Fatalf("got ok=%v err=%v, want a predecessor", ok, err)
	}
	if pred.ID != one.ID {
		t.Errorf("predecessor id = %d, want book 1 (id %d) - book 2 lacks knowledge-final.md", pred.ID, one.ID)
	}
}
