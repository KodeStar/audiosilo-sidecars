package metaops

import (
	"encoding/json"
	"os"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
)

const scanCacheVersion = 1

// scanCacheFile is deliberately private: it is daemon state, not an API
// contract. Only a successful, complete snapshot is stored, so an interrupted or
// failed scan can never displace the last useful Library result.
type scanCacheFile struct {
	Version int     `json:"version"`
	Job     ScanJob `json:"job"`
}

// persistCache atomically writes the newest successful job. Cache failures are
// best-effort: losing this optimization must not fail a scan or the daemon.
func (m *ScanManager) persistCache() {
	if m.cachePath == "" {
		return
	}

	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	m.mu.Lock()
	var newest *scanJob
	for _, job := range m.jobs {
		if job.status == ScanDone && (newest == nil || job.seq > newest.seq) {
			newest = job
		}
	}
	if newest == nil {
		m.mu.Unlock()
		return
	}
	snapshot := m.snapshotLocked(newest)
	m.mu.Unlock()

	raw, err := json.Marshal(scanCacheFile{Version: scanCacheVersion, Job: snapshot})
	if err != nil {
		return
	}
	_ = fsutil.WriteFileAtomic(m.cachePath, append(raw, '\n'), 0o600)
}

// restoreCache recreates one completed in-memory job from the last successful
// snapshot. Invalid/old/truncated files are ignored; the user can simply scan
// again. Cached manual matches and hidden flags become read-time patches so a
// later unhide or cleared match can supersede them without another daemon restart.
func (m *ScanManager) restoreCache() {
	if m.cachePath == "" {
		return
	}
	raw, err := os.ReadFile(m.cachePath) //nolint:gosec // configured daemon data path
	if err != nil {
		return
	}
	var cached scanCacheFile
	if json.Unmarshal(raw, &cached) != nil || cached.Version != scanCacheVersion ||
		cached.Job.Status != ScanDone || cached.Job.ID == "" || cached.Job.Path == "" {
		return
	}
	startedAt, err := time.Parse(time.RFC3339, cached.Job.StartedAt)
	if err != nil {
		return
	}

	job := &scanJob{
		id:          cached.Job.ID,
		seq:         1,
		path:        cached.Job.Path,
		status:      ScanDone,
		startedAt:   startedAt,
		phase:       "done",
		walkDirs:    cached.Job.Progress.WalkDirs,
		walkGroups:  cached.Job.Progress.WalkGroups,
		groupsDone:  cached.Job.Progress.GroupsDone,
		groupsTotal: cached.Job.Progress.GroupsTotal,
		books:       cached.Job.Books,
		idents:      make(map[string]bookIdent, len(cached.Job.Books)),
		dispatched:  make(map[string]string, len(cached.Job.Books)),
		resolved:    make(map[string]resolvedCov, len(cached.Job.Books)),
		stats:       cached.Job.Stats,
		sem:         make(chan struct{}, coverageWorkers),
	}
	for i := range job.books {
		book := &job.books[i]
		bi := newBookIdent(BookIdentity{
			ASIN: book.ASIN, ISBN: book.ISBN, Title: book.Title, Authors: book.Authors,
			Series: book.Series, SeriesPos: book.SeriesPosition,
		}, "")
		job.idents[book.Path] = bi
		job.dispatched[book.Path] = bi.fp

		coverage := book.Coverage
		book.Coverage = Coverage{}
		job.resolved[book.Path] = resolvedCov{fingerprint: bi.fp, coverage: coverage}

		// Move user-controlled state out of the cached base and into an explicit
		// overlay. That makes a later false/nil ApplyOverride authoritative.
		patch := overridePatch{hidden: book.Hidden}
		book.Hidden = false
		if coverage.MatchedBy == "manual" {
			manual := coverage
			patch.coverage = &manual
			job.resolved[book.Path] = resolvedCov{fingerprint: bi.fp}
		}
		m.patches[book.SourcePath] = patch
	}

	m.seq = 1
	m.jobs[job.id] = job
}
