package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/ebook"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// TestEbookFullMachineToDone drives a REAL epub the whole way to done.
//
// The agent is faked, so this proves the WIRING rather than the authoring: that
// an ebook book routes through its own front half, that every artifact the shared
// authoring tail expects is on disk in the right shape, that the tail runs against
// them unchanged, and that the book's source prose does not survive completion.
// What it cannot prove - whether the sidecars are any good - needs a real agent
// and is a separate, paid exercise.
//
// It uses a public-domain epub from the maintainer's corpus, env-gated on
// AUDIOSILO_EPUB_DIR, so no book enters the repo. It builds a small synthetic epub
// instead when the corpus is absent, which keeps the test meaningful in CI.
func TestEbookFullMachineToDone(t *testing.T) {
	dir := t.TempDir()
	src := corpusEpubOrSynthetic(t, dir)
	workRoot := filepath.Join(dir, "work")

	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hub := events.NewHub(1024)

	fake := newFakeRunner()
	fake.act = fullFakeAct(t, fullFakeOpts{title: "An Ebook"})
	cfg := fullFakeConfig(dir, fake)
	cfg.DB = db
	cfg.Fallback = scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond)
	exe := NewExecutor(cfg)
	// autoPurge ON: reclaiming an ebook's text when it reaches done is a copyright
	// requirement, not just disk hygiene, so it is part of what this test proves.
	sched := scheduler.New(db, hub, exe, 2, workRoot, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = sched.Start(ctx) }()

	workDir := filepath.Join(workRoot, "an-ebook")
	b, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: src,
		WorkDir:    workDir,
		Title:      "An Ebook",
		Kind:       "ebook",
		EbookPath:  src,
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	sched.Notify()

	final := waitState(t, db, b.ID, "done", 90*time.Second)
	cancel()
	<-done

	if final.State != "done" {
		t.Fatalf("book state = %q (status %q, err %q), want done", final.State, final.Status, final.Error)
	}

	// The ebook front half ran, and the AUDIO front half did not. A stage_run for
	// asr or splitting would mean the state machine routed by something other than
	// the book's kind.
	ran, err := db.SucceededStages(context.Background(), b.ID)
	if err != nil {
		t.Fatalf("succeeded stages: %v", err)
	}
	if !ran["extracting"] {
		t.Errorf("extracting did not run; stages = %v", ran)
	}
	for _, audioOnly := range []string{"inspecting", "splitting", "asr", "sanitizing", "qa_sweep", "spelling_research", "correcting"} {
		if ran[audioOnly] {
			t.Errorf("audio stage %q ran for an ebook; stages = %v", audioOnly, ran)
		}
	}
	// The authoring tail is shared, so it must have run unchanged.
	for _, shared := range []string{"fact_pass", "synthesizing", "validating", "auditing", "contributing"} {
		if !ran[shared] {
			t.Errorf("shared stage %q did not run; stages = %v", shared, ran)
		}
	}

	// The sidecars exist - the point of the whole pipeline.
	for _, f := range []string{charactersFileName, recapsFileName} {
		if _, err := os.Stat(filepath.Join(workDir, sidecarsDir, f)); err != nil {
			t.Errorf("sidecar %s missing: %v", f, err)
		}
	}

	// The book's own prose must NOT survive completion. Auto-purge runs when a book
	// reaches done, and for an ebook that is a copyright requirement, not just disk
	// hygiene: the source text may not outlive the derivation.
	for _, d := range []string{ebook.TextDir, ebook.ExtractDir} {
		if _, err := os.Stat(filepath.Join(workDir, d)); err == nil {
			t.Errorf("%s/ still exists after done; the book's source text must be purged", d)
		}
	}
	// The durables that make the result auditable DO survive.
	for _, f := range []string{"manifest.json", ebook.ManifestName, filepath.Join("facts", "knowledge-final.md")} {
		if _, err := os.Stat(filepath.Join(workDir, f)); err != nil {
			t.Errorf("durable %s was purged: %v", f, err)
		}
	}

	// books.words was recorded, so the Running list can size an ebook that has no
	// duration to show.
	got, err := db.GetBook(context.Background(), b.ID)
	if err != nil {
		t.Fatalf("get book: %v", err)
	}
	if got.Words <= 0 {
		t.Errorf("books.words = %d, want the extracted word count", got.Words)
	}
	if got.Chapters <= 0 {
		t.Errorf("books.chapters = %d, want the mapped chapter count", got.Chapters)
	}
	if got.DurationSec != 0 {
		t.Errorf("books.duration_sec = %v, want 0 - an epub has no runtime", got.DurationSec)
	}
}

// corpusEpubOrSynthetic returns a real epub from the maintainer's library when
// AUDIOSILO_EPUB_DIR points at one, else a small synthetic epub. The corpus books
// are copyrighted and never enter the repo, so the synthetic fallback is what keeps
// this test running in CI.
func corpusEpubOrSynthetic(t *testing.T, dir string) string {
	t.Helper()
	if root := os.Getenv("AUDIOSILO_EPUB_DIR"); root != "" {
		if strings.HasPrefix(root, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				root = filepath.Join(home, strings.TrimPrefix(root, "~"))
			}
		}
		// Prefer a public-domain book: it is the one class of corpus file whose
		// content could be quoted in a failure message without concern.
		var found string
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || found != "" || !ebook.IsEpub(p) {
				return nil //nolint:nilerr // an unreadable subtree must not fail the sweep
			}
			if strings.Contains(strings.ToLower(p), "public-domain") {
				found = p
			}
			return nil
		})
		if found != "" {
			t.Logf("driving the real corpus epub %s", filepath.Base(found))
			return found
		}
	}
	epub := filepath.Join(dir, "synthetic.epub")
	buildTestEpub(t, epub, [][2]string{
		{"Cover", words(4)},
		{"Chapter 1: The Start", words(1400)},
		{"Chapter 2: The Middle", words(1500)},
		{"Chapter 3: The End", words(1300)},
		{"About the Author", words(25)},
	})
	return epub
}
