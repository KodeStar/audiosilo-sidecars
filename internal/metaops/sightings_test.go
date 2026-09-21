package metaops

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	metascan "github.com/kodestar/audiosilo-meta/pkg/scan"
)

// sightingRow is what fakeSightings remembers per path, mirroring the store's
// semantics: a known path keeps its first_seen_at and baseline flag.
type sightingRow struct {
	firstSeenAt string
	baseline    bool
}

// fakeSightings is an in-memory SightingRecorder with the store's semantics: a
// known path keeps its first_seen_at and baseline flag and only bumps last_seen.
type fakeSightings struct {
	mu   sync.Mutex
	rows map[string]sightingRow
	// recorded counts RecordSightings calls, so a test can prove a completed scan
	// wrote exactly once.
	recorded int
	// baselines records the baseline flag of each batch, in order.
	baselines []bool
}

func newFakeSightings() *fakeSightings { return &fakeSightings{rows: map[string]sightingRow{}} }

func (f *fakeSightings) RecordSightings(_ context.Context, paths []string, at time.Time, baseline bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded++
	f.baselines = append(f.baselines, baseline)
	for _, p := range paths {
		if _, ok := f.rows[p]; ok {
			continue
		}
		f.rows[p] = sightingRow{firstSeenAt: at.UTC().Format(time.RFC3339), baseline: baseline}
	}
	return nil
}

func (f *fakeSightings) HasSightings(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows) > 0, nil
}

func (f *fakeSightings) snapshot() map[string]sightingRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]sightingRow, len(f.rows))
	for k, v := range f.rows {
		out[k] = v
	}
	return out
}

// TestScanManagerRecordsSightings covers the recording seam: a completed scan
// records every candidate path, the first batch is the baseline, and a later scan
// neither re-baselines nor resets a known path's first_seen_at.
func TestScanManagerRecordsSightings(t *testing.T) {
	root := t.TempDir()
	rec := newFakeSightings()
	m := NewScanManager(context.Background(), NewClient(""), "", nil, WithSightings(rec))
	m.scan = fakeScan([]metascan.Book{
		{Path: "Author/One", Title: "One", AudioFiles: 1},
		{Path: "Author/Two", Title: "Two", AudioFiles: 1},
	}, metascan.Stats{Books: 2})

	id, err := m.Start(root)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)
	if job.Status != ScanDone {
		t.Fatalf("status = %q (%s)", job.Status, job.Error)
	}

	rows := rec.snapshot()
	if len(rows) != 2 {
		t.Fatalf("recorded sightings = %+v, want both candidates", rows)
	}
	if rec.recorded != 1 {
		t.Fatalf("RecordSightings calls = %d, want exactly one per completed scan", rec.recorded)
	}
	// The FIRST batch ever recorded is the baseline, so an upgrade does not report
	// the library the user already owns as new.
	if len(rec.baselines) != 1 || !rec.baselines[0] {
		t.Fatalf("baseline flags = %v, want one baseline batch", rec.baselines)
	}
	for _, b := range job.Books {
		row, ok := rows[b.SourcePath]
		if !ok {
			t.Errorf("candidate %q was not recorded", b.SourcePath)
			continue
		}
		if _, err := time.Parse(time.RFC3339, row.firstSeenAt); err != nil {
			t.Errorf("first_seen_at %q is not RFC3339: %v", row.firstSeenAt, err)
		}
	}

	// A second scan does NOT re-baseline, and a path seen before keeps its original
	// first_seen_at while a genuinely new folder is recorded as non-baseline.
	m.scan = fakeScan([]metascan.Book{
		{Path: "Author/One", Title: "One", AudioFiles: 1},
		{Path: "Author/Three", Title: "Three", AudioFiles: 1},
	}, metascan.Stats{Books: 2})
	id2, err := m.Start(root)
	if err != nil {
		t.Fatal(err)
	}
	second := waitDone(t, m, id2)
	if len(rec.baselines) != 2 || rec.baselines[1] {
		t.Fatalf("baseline flags = %v, want the second batch non-baseline", rec.baselines)
	}
	rows = rec.snapshot()
	if got := rows[filepath.Join(second.Path, "Author", "Three")]; got.firstSeenAt == "" || got.baseline {
		t.Fatalf("newly-appeared folder = %+v, want a non-baseline sighting", got)
	}
	if got := rows[filepath.Join(second.Path, "Author", "One")]; !got.baseline {
		t.Fatalf("known folder lost its baseline flag: %+v", got)
	}
}

// TestScanManagerWithoutSightingsRecorderStillScans pins the nil-recorder path:
// the feature is injected, and a manager without it must scan exactly as before
// (no first_seen_at, no panic).
func TestScanManagerWithoutSightingsRecorderStillScans(t *testing.T) {
	m := NewScanManager(context.Background(), NewClient(""), "", nil)
	m.scan = fakeScan([]metascan.Book{{Path: "A/B", Title: "B", AudioFiles: 1}}, metascan.Stats{Books: 1})
	id, err := m.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := waitDone(t, m, id)
	if job.Status != ScanDone || len(job.Books) != 1 {
		t.Fatalf("job = %+v", job)
	}
	if job.Books[0].FirstSeenAt != "" {
		t.Errorf("first_seen_at = %q with no recorder, want empty", job.Books[0].FirstSeenAt)
	}
}

func TestComputeIsNew(t *testing.T) {
	queued := &PipelineBookRef{ID: 1, State: "asr", Status: "active"}

	cases := []struct {
		name         string
		book         ScannedBook
		baseline     bool
		acknowledged bool
		want         bool
	}{
		{
			name: "plainly new",
			book: ScannedBook{SourcePath: "/lib/a"},
			want: true,
		},
		{
			name:     "baseline is the library as it already stood",
			book:     ScannedBook{SourcePath: "/lib/a"},
			baseline: true,
			want:     false,
		},
		{
			name:         "acknowledged is dismissed",
			book:         ScannedBook{SourcePath: "/lib/a"},
			acknowledged: true,
			want:         false,
		},
		{
			name: "already queued or processed here",
			book: ScannedBook{SourcePath: "/lib/a", PipelineBook: queued},
			want: false,
		},
		{
			name: "hidden candidates never surface",
			book: ScannedBook{SourcePath: "/lib/a", Hidden: true},
			want: false,
		},
		{
			name:         "baseline AND acknowledged is still not new",
			book:         ScannedBook{SourcePath: "/lib/a"},
			baseline:     true,
			acknowledged: true,
			want:         false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeIsNew(tc.book, tc.baseline, tc.acknowledged); got != tc.want {
				t.Fatalf("ComputeIsNew = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRestoredCacheSeedsTheBaseline is the upgrade path: a daemon that restarts
// with a cached scan and an empty sighting table baselines that cached library at
// startup, so the New view starts empty instead of reporting every folder the
// user owns.
func TestRestoredCacheSeedsTheBaseline(t *testing.T) {
	root := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "library-scan-cache.json")
	// The scan that fills the cache runs with NO recorder, exactly like a daemon
	// predating the feature.
	m := NewScanManager(context.Background(), NewClient(""), "", nil, WithScanCache(cachePath))
	m.scan = fakeScan([]metascan.Book{
		{Path: "Author/One", Title: "One", AudioFiles: 1},
		{Path: "Author/Two", Title: "Two", AudioFiles: 1},
	}, metascan.Stats{Books: 2})
	id, err := m.Start(root)
	if err != nil {
		t.Fatal(err)
	}
	cached := waitDone(t, m, id)
	waitForCache(t, cachePath)

	rec := newFakeSightings()
	restored := NewScanManager(context.Background(), NewClient(""), "", nil,
		WithScanCache(cachePath), WithSightings(rec))
	rows := rec.snapshot()
	if len(rows) != 2 {
		t.Fatalf("seeded sightings = %+v, want both cached candidates", rows)
	}
	for path, row := range rows {
		if !row.baseline {
			t.Errorf("seeded %q is not baseline: %+v", path, row)
		}
		if row.firstSeenAt != cached.StartedAt {
			t.Errorf("seeded %q first_seen_at = %q, want the cached job's started_at %q",
				path, row.firstSeenAt, cached.StartedAt)
		}
	}
	// The restored job's books carry no sighting state of their own - the API
	// attaches it per read - so the seeded baseline is what makes them not new.
	job, ok := restored.Get(id)
	if !ok {
		t.Fatal("restored job is not addressable")
	}
	for _, b := range job.Books {
		if ComputeIsNew(b, rows[b.SourcePath].baseline, false) {
			t.Errorf("restored baseline candidate %q reports new", b.SourcePath)
		}
	}
}

// waitForCache blocks until the scan cache file exists.
func waitForCache(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("completed scan cache was not written")
		}
		time.Sleep(time.Millisecond)
	}
}
