package metaops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	metascan "github.com/kodestar/audiosilo-meta/pkg/scan"
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
	// Disabled coverage client (no base URL) + ffprobe disabled + no overrides.
	m := NewScanManager(context.Background(), NewClient(""), "", nil)

	id, err := m.Start(root)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)
	if job.Status != ScanDone {
		t.Fatalf("status = %q (err %q)", job.Status, job.Error)
	}
	if len(job.Books) != 3 {
		t.Fatalf("expected 3 books, got %+v", job.Books)
	}
	if job.Progress.Phase != "done" || job.Progress.BooksFound != 3 || job.Progress.CoverageDone != 3 {
		t.Fatalf("progress = %+v", job.Progress)
	}
	if job.StartedAt == "" {
		t.Error("job missing started_at")
	}
	if job.Stats == nil || job.Stats.Books != 3 {
		t.Fatalf("stats = %+v", job.Stats)
	}
	for _, b := range job.Books {
		if b.Coverage.Available {
			t.Errorf("book %q coverage should be unavailable (disabled client): %+v", b.Title, b.Coverage)
		}
		if b.Title == "" || b.AudioFiles == 0 {
			t.Errorf("book missing basic fields: %+v", b)
		}
	}
}

// fakeScan drives OnProgress + OnBook deterministically then returns a fixed
// authoritative result, so the manager's streaming/replace lifecycle is testable
// without a real filesystem walk.
func fakeScan(books []metascan.Book, stats metascan.Stats) scanFunc {
	return func(root string, opts metascan.Options) (*metascan.Result, metascan.Stats, error) {
		if opts.OnWalk != nil {
			// The walk phase reports dirs/groups BEFORE OnProgress/OnBook fire.
			opts.OnWalk(len(books)*2, len(books))
		}
		if opts.OnProgress != nil {
			opts.OnProgress(0, len(books))
		}
		for i, b := range books {
			if opts.OnBook != nil {
				opts.OnBook(b)
			}
			if opts.OnProgress != nil {
				opts.OnProgress(i+1, len(books))
			}
		}
		return &metascan.Result{Root: root, Books: books}, stats, nil
	}
}

func TestScanManagerStreamsAndResolvesCoverage(t *testing.T) {
	// A fake meta server that knows one of the two books by ASIN.
	s := &metaServer{
		lookup: map[string]string{"B-KNOWN": "w-known"},
		work:   map[string]workRow{"w-known": {title: "Known Work", c: true}},
	}
	c, _ := newMeta(t, s)
	m := NewScanManager(context.Background(), c, "", nil)
	m.scan = fakeScan([]metascan.Book{
		{Path: "/lib/known", Title: "Known", Authors: []string{"A"}, ASIN: "B-KNOWN", AudioFiles: 1},
		{Path: "/lib/unknown", Title: "Unknown", Authors: []string{"B"}, AudioFiles: 1},
	}, metascan.Stats{Books: 2})

	id, err := m.Start(t.TempDir())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)
	if job.Status != ScanDone {
		t.Fatalf("status = %q (%q)", job.Status, job.Error)
	}
	if len(job.Books) != 2 || job.Progress.GroupsTotal != 2 || job.Progress.GroupsDone != 2 {
		t.Fatalf("groups/books = %+v / %d books", job.Progress, len(job.Books))
	}
	if job.Progress.CoverageDone != 2 || job.Progress.CoverageTotal != 2 {
		t.Fatalf("coverage counters = %+v", job.Progress)
	}
	byPath := map[string]ScannedBook{}
	for _, b := range job.Books {
		byPath[b.Path] = b
	}
	if k := byPath["/lib/known"]; !k.Coverage.Known || k.Coverage.MatchedBy != "asin" || !k.Coverage.HasCharacters {
		t.Fatalf("known book coverage = %+v", k.Coverage)
	}
	if u := byPath["/lib/unknown"]; u.Coverage.Known {
		t.Fatalf("unknown book should not be known: %+v", u.Coverage)
	}
}

// TestScanManagerReportsWalkProgress proves the OnWalk callback updates the job's
// walk counters and they surface on the progress snapshot WHILE the scan is still
// running (before groups_total is known). The scan func blocks after reporting the
// walk so the assertion observes the running snapshot deterministically.
func TestScanManagerReportsWalkProgress(t *testing.T) {
	walked := make(chan struct{})
	proceed := make(chan struct{})
	m := NewScanManager(context.Background(), NewClient(""), "", nil)
	m.scan = func(root string, opts metascan.Options) (*metascan.Result, metascan.Stats, error) {
		if opts.OnWalk != nil {
			opts.OnWalk(7, 3)
		}
		close(walked)
		<-proceed
		return &metascan.Result{Root: root}, metascan.Stats{}, nil
	}

	id, err := m.Start(t.TempDir())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-walked

	job, ok := m.Get(id)
	if !ok {
		t.Fatal("job vanished")
	}
	if job.Status != ScanRunning {
		t.Fatalf("status = %q, want running", job.Status)
	}
	if job.Progress.WalkDirs != 7 || job.Progress.WalkGroups != 3 {
		t.Fatalf("walk counters = %+v, want dirs=7 groups=3", job.Progress)
	}
	if job.Progress.GroupsTotal != 0 {
		t.Fatalf("groups_total = %d, want 0 (walk not yet done)", job.Progress.GroupsTotal)
	}

	close(proceed)
	if done := waitDone(t, m, id); done.Status != ScanDone {
		t.Fatalf("final status = %q (%q)", done.Status, done.Error)
	}
}

func TestScanManagerAppliesOverrides(t *testing.T) {
	s := &metaServer{work: map[string]workRow{"w-manual": {title: "Manual Pick", r: true}}}
	c, _ := newMeta(t, s)
	// Persisted overrides key on the ABSOLUTE, canonical source path, while metascan
	// books carry root-relative paths - the manager joins them onto the canonical
	// root (a live-smoke bug: a relative-key lookup silently applied nothing). Start
	// canonicalizes its root, so the override keys must use the canonical form too.
	root := t.TempDir()
	canonRoot, _ := resolvePath(root)
	overrides := OverrideLookup(func(context.Context) (map[string]Override, error) {
		return map[string]Override{
			filepath.Join(canonRoot, "Author/hidden"): {Hidden: true},
			filepath.Join(canonRoot, "Author/manual"): {WorkID: "w-manual"},
		}, nil
	})
	m := NewScanManager(context.Background(), c, "", overrides)
	m.scan = fakeScan([]metascan.Book{
		{Path: "Author/hidden", Title: "Hide Me", AudioFiles: 1},
		{Path: "Author/manual", Title: "Match Me", AudioFiles: 1},
		{Path: "Author/plain", Title: "Plain", AudioFiles: 1},
	}, metascan.Stats{Books: 3})

	id, _ := m.Start(root)
	job := waitDone(t, m, id)
	byPath := map[string]ScannedBook{}
	for _, b := range job.Books {
		byPath[b.Path] = b
	}
	if !byPath["Author/hidden"].Hidden {
		t.Errorf("override hide not applied: %+v", byPath["Author/hidden"])
	}
	man := byPath["Author/manual"]
	if !man.Coverage.Known || man.Coverage.MatchedBy != "manual" || man.Coverage.WorkID != "w-manual" || !man.Coverage.HasRecaps {
		t.Fatalf("manual override coverage = %+v", man.Coverage)
	}
	if byPath["Author/plain"].Hidden {
		t.Errorf("plain book should not be hidden: %+v", byPath["Author/plain"])
	}
	if got := byPath["Author/plain"].SourcePath; got != filepath.Join(canonRoot, "Author/plain") {
		t.Fatalf("source_path not absolutized: %q", got)
	}

	// A live override reflects at read time on the already-completed job. Live
	// patches key on the absolute (canonical) source path too.
	m.ApplyOverride(filepath.Join(canonRoot, "Author/plain"), true, nil)
	job, _ = m.Get(id)
	for _, b := range job.Books {
		if b.Path == "Author/plain" && !b.Hidden {
			t.Errorf("live ApplyOverride(hide) not reflected: %+v", b)
		}
	}
	// Clearing it removes the patch.
	m.ApplyOverride(filepath.Join(canonRoot, "Author/plain"), false, nil)
	job, _ = m.Get(id)
	for _, b := range job.Books {
		if b.Path == "Author/plain" && b.Hidden {
			t.Errorf("cleared override still hides book: %+v", b)
		}
	}
}

// TestResolveWorkerDropsStaleFingerprint is the race regression: when
// corroboration re-identifies a book (F1 -> F2) between dispatch and applyFinal,
// a stale F1 worker finishing LAST must not clobber F2's fresh verdict (which
// would wedge the fp-gated readers - coverage never applies and coverage_done
// never reaches the total).
func TestResolveWorkerDropsStaleFingerprint(t *testing.T) {
	s := &metaServer{
		lookup: map[string]string{"B-1": "w1", "B-2": "w2"},
		work: map[string]workRow{
			"w1": {title: "One", c: true},
			"w2": {title: "Two", r: true},
		},
	}
	c, _ := newMeta(t, s)
	m := NewScanManager(context.Background(), c, "", nil)

	const path = "some/book"
	job := &scanJob{
		idents:     map[string]bookIdent{},
		dispatched: map[string]string{},
		resolved:   map[string]resolvedCov{},
		sem:        make(chan struct{}, coverageWorkers),
		books:      []ScannedBook{{Path: path}},
	}
	bi1 := newBookIdent(BookIdentity{ASIN: "B-1"}, "") // stale
	bi2 := newBookIdent(BookIdentity{ASIN: "B-2"}, "") // fresh, re-identified

	// The book's current identity is F2 (a fresh dispatch already happened).
	job.idents[path] = bi2
	job.dispatched[path] = bi2.fp

	// F2's verdict lands first (the fresh identity)...
	job.wg.Add(1)
	m.resolveWorker(job, path, bi2.fp, bi2)
	// ...then F1's stale worker finishes last - it must be DROPPED, not clobber F2.
	job.wg.Add(1)
	m.resolveWorker(job, path, bi1.fp, bi1)

	m.mu.Lock()
	rc, ok := job.resolved[path]
	m.mu.Unlock()
	if !ok {
		t.Fatal("no verdict recorded")
	}
	if rc.fingerprint != bi2.fp {
		t.Fatalf("stale F1 verdict clobbered F2: fingerprint = %q, want %q", rc.fingerprint, bi2.fp)
	}
	// w2 has recaps (not characters); w1 the opposite - proves F2's verdict survived.
	if !rc.coverage.HasRecaps || rc.coverage.HasCharacters {
		t.Fatalf("surviving coverage is not F2's (w2 has recaps): %+v", rc.coverage)
	}
	// The fp-gated progress reader counts it: coverage_done reaches the total.
	if p := m.progressLocked(job); p.CoverageDone != 1 || p.CoverageTotal != 1 {
		t.Fatalf("coverage_done did not count the fresh verdict: %+v", p)
	}
}

// TestScanManagerCanonicalizesRoot proves Start canonicalizes its root so a
// trailing-slash spelling still yields canonical (clean, symlink-eval'd)
// SourcePath values - the durable key a stored override must match.
func TestScanManagerCanonicalizesRoot(t *testing.T) {
	root := t.TempDir()
	m := NewScanManager(context.Background(), NewClient(""), "", nil)
	m.scan = fakeScan([]metascan.Book{
		{Path: "Author/Book", Title: "B", AudioFiles: 1},
	}, metascan.Stats{Books: 1})

	// Start with a trailing-slash spelling of the root.
	id, err := m.Start(root + string(filepath.Separator))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)

	// The canonical form (symlink-eval'd, e.g. /var -> /private/var on macOS) is
	// what both the displayed root and every SourcePath must carry.
	canonRoot, _ := resolvePath(root)
	if job.Path != canonRoot {
		t.Fatalf("job.Path = %q, want canonical %q", job.Path, canonRoot)
	}
	want := filepath.Join(canonRoot, "Author/Book")
	if got := job.Books[0].SourcePath; got != want {
		t.Fatalf("SourcePath = %q, want canonical %q", got, want)
	}
}

func TestScanManagerListAndRetention(t *testing.T) {
	m := NewScanManager(context.Background(), NewClient(""), "", nil)
	m.scan = fakeScan(nil, metascan.Stats{})

	// Run more than the retention cap so old finished jobs are pruned.
	var lastID string
	for range retainedFinished + 5 {
		id, err := m.Start(t.TempDir())
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitDone(t, m, id)
		lastID = id
	}
	list := m.List()
	if len(list) != retainedFinished {
		t.Fatalf("List retained %d, want %d", len(list), retainedFinished)
	}
	// Newest-first: the most recently started job heads the list.
	if list[0].ID != lastID {
		t.Errorf("List not newest-first: head=%q want %q", list[0].ID, lastID)
	}
	// Summaries carry no book list (that shape is the get endpoint's).
	for _, s := range list {
		if s.Status != ScanDone || s.StartedAt == "" {
			t.Errorf("summary = %+v", s)
		}
	}
}

func TestScanManagerRunnerError(t *testing.T) {
	m := NewScanManager(context.Background(), NewClient(""), "", nil)
	m.scan = func(string, metascan.Options) (*metascan.Result, metascan.Stats, error) {
		return nil, metascan.Stats{}, fmt.Errorf("boom")
	}
	id, err := m.Start(t.TempDir())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)
	if job.Status != ScanError || job.Error != "boom" {
		t.Fatalf("expected error status, got %+v", job)
	}
}

func TestScanManagerRejectsBadPath(t *testing.T) {
	m := NewScanManager(context.Background(), NewClient(""), "", nil)
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

// TestScanManagerRealScanFixtureStable exercises the real metascan.Scan over a
// many-book fixture: every book resolves coverage and the authoritative list is
// stable across runs (the final sort is deterministic).
func TestScanManagerRealScanFixtureStable(t *testing.T) {
	root := t.TempDir()
	const n = 12
	for i := range n {
		d := filepath.Join(root, fmt.Sprintf("Book %02d", i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "audio.m4b"), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := newMeta(t, &metaServer{})
	m := NewScanManager(context.Background(), c, "", nil)

	run := func() []ScannedBook {
		id, err := m.Start(root)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		job := waitDone(t, m, id)
		if job.Status != ScanDone || len(job.Books) != n {
			t.Fatalf("status=%q books=%d", job.Status, len(job.Books))
		}
		if job.Progress.CoverageDone != n {
			t.Fatalf("coverage_done = %d, want %d", job.Progress.CoverageDone, n)
		}
		return job.Books
	}

	first := run()
	for _, b := range first {
		if !b.Coverage.Available {
			t.Errorf("book %q coverage not resolved: %+v", b.Title, b.Coverage)
		}
	}
	second := run()
	for i := range first {
		if first[i].Path != second[i].Path {
			t.Fatalf("scan order not stable at %d: %q vs %q", i, first[i].Path, second[i].Path)
		}
	}
}
