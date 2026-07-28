package ebook

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/extract"
)

// TestCorpusUniverse runs the real split + chapter derivation over a local epub
// library and reports what the pipeline would actually do with each book.
//
// Env-gated on AUDIOSILO_EPUB_DIR because the corpus is copyrighted and can never
// enter the repo - the same arrangement the historical-extraction golden tests use.
// It writes only into t.TempDir().
//
// The number to watch is not just how many books come out contiguous (those skip
// the chapter-mapping agent) but how many quarantine a suspected excerpt: letting
// another book's chapter reach the fact pass is the failure this package exists to
// prevent, and it is invisible downstream.
func TestCorpusUniverse(t *testing.T) {
	dir := os.Getenv("AUDIOSILO_EPUB_DIR")
	if dir == "" {
		t.Skip("AUDIOSILO_EPUB_DIR not set; skipping the real-corpus measurement")
	}
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("resolve ~: %v", err)
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}

	var epubs []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && IsEpub(p) {
			epubs = append(epubs, p)
		}
		return nil
	})
	if len(epubs) == 0 {
		t.Skipf("no .epub files under %s", dir)
	}
	sort.Strings(epubs)

	var readable, contiguous, suspected, mappable, parked int
	for _, p := range epubs {
		man, err := extract.Split(p, t.TempDir())
		if err != nil {
			t.Logf("%-50s UNREADABLE (would park ebook_unreadable): %v", base(p), err)
			continue
		}
		readable++
		u := BuildUniverse(man)

		route := "chapter_mapping"
		switch {
		case u.Contiguous:
			route = "-> fact_pass"
			contiguous++
		case u.Labeled >= 2:
			mappable++
		default:
			route = "PARK ebook_no_chapters"
			parked++
		}
		flag := ""
		if u.Suspected {
			suspected++
			flag = "  EXCERPT-QUARANTINED"
		}
		t.Logf("%-50s docs=%3d chapters=%3d words=%7d %-16s%s",
			base(p), len(u.Docs), len(u.Chapters), u.Words, route, flag)
	}

	t.Logf("")
	t.Logf("CORPUS UNIVERSE: %d readable | %d straight to fact_pass | %d need the mapping agent | %d park | %d quarantined a suspected excerpt",
		readable, contiguous, mappable, parked, suspected)

	if readable == 0 {
		t.Fatal("no epub in the corpus produced a universe")
	}
	// Every readable book must be routable: contiguous, mappable by the agent, or
	// explicitly parked. A book that is none of those would stall.
	if contiguous+mappable+parked != readable {
		t.Errorf("routing accounts for %d of %d books", contiguous+mappable+parked, readable)
	}
}

func base(p string) string {
	b := filepath.Base(p)
	if len(b) > 50 {
		return b[:50]
	}
	return b
}
