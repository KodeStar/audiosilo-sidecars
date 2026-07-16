package metaops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	metascan "github.com/kodestar/audiosilo-meta/pkg/scan"
)

// coverageWorkers bounds concurrent per-book coverage lookups during a scan, so a
// large folder resolves coverage in parallel without opening an unbounded number
// of HTTP connections. The shared client's TTL caches are mutex-safe.
const coverageWorkers = 8

// retainedFinished caps how many finished jobs List keeps (in addition to every
// still-running job), so the in-memory job map cannot grow without bound while a
// reloaded UI can still reattach to recent scans.
const retainedFinished = 10

// ScanStatus is a job's lifecycle state.
type ScanStatus string

const (
	// ScanRunning: the scan (or coverage merge) is in progress.
	ScanRunning ScanStatus = "running"
	// ScanDone: the result is available.
	ScanDone ScanStatus = "done"
	// ScanError: the scan failed (e.g. the path vanished); Error is set.
	ScanError ScanStatus = "error"
)

// Override is the per-book override the scan applies: a manual hide and/or a
// manual work match. It mirrors the store's persisted shape but keeps metaops
// store-agnostic (only the meta module + stdlib + pkg/match).
type Override struct {
	Hidden    bool
	WorkID    string
	WorkTitle string
}

// OverrideLookup supplies the current set of candidate overrides (keyed by a
// book's source path) at scan start, so hidden/manually-matched books are honored
// from the DB without metaops importing the store. It may be nil (no overrides).
type OverrideLookup func(ctx context.Context) (map[string]Override, error)

// scanFunc is the folder-scan entry point, injectable so tests drive the manager
// without a real filesystem walk. It defaults to metascan.Scan.
type scanFunc func(root string, opts metascan.Options) (*metascan.Result, metascan.Stats, error)

// ScannedBook is one detected book plus its merged coverage verdict. The scan
// fields are hand-mirrored from metascan.Book (the meta module contract) so the
// API DTO is stable independent of that struct's private helpers.
type ScannedBook struct {
	// Path is the book folder relative to the scan root (display + in-scan key).
	Path string `json:"path"`
	// SourcePath is the ABSOLUTE book folder - the durable identity every API
	// call keys on (POST /books candidates, overrides, PathAllowed). The client
	// must never derive it by joining paths itself.
	SourcePath     string   `json:"source_path"`
	Title          string   `json:"title"`
	Subtitle       string   `json:"subtitle,omitempty"`
	Authors        []string `json:"authors,omitempty"`
	Narrators      []string `json:"narrators,omitempty"`
	Series         string   `json:"series,omitempty"`
	SeriesPosition string   `json:"series_position,omitempty"`
	ASIN           string   `json:"asin,omitempty"`
	ISBN           string   `json:"isbn,omitempty"`
	RuntimeMin     int      `json:"runtime_min,omitempty"`
	Chapters       int      `json:"chapters,omitempty"`
	AudioFiles     int      `json:"audio_files"`
	// Hidden is true when a persisted (or live) override hides this book from the
	// default candidate list.
	Hidden bool `json:"hidden,omitempty"`
	// Sources records where each field came from ("tag" | "path" | "filename").
	Sources map[string]string `json:"sources,omitempty"`
	// Coverage is the metadata verdict (Available/Known/HasCharacters/HasRecaps).
	Coverage Coverage `json:"coverage"`
}

// ScanProgress is the fine-grained progress of a scan job. The folder walk drives
// groups_done/groups_total (one group per directory); coverage resolution drives
// coverage_done/coverage_total; books_found grows as books stream in. The phase
// is display-only - the counters are the authoritative story.
type ScanProgress struct {
	Phase         string `json:"phase"` // "scanning" | "coverage" | "done"
	GroupsDone    int    `json:"groups_done"`
	GroupsTotal   int    `json:"groups_total"`
	BooksFound    int    `json:"books_found"`
	CoverageDone  int    `json:"coverage_done"`
	CoverageTotal int    `json:"coverage_total"`
}

// ScanJob is an async folder-scan job snapshot (safe to serialize). Books is the
// candidate list, growing while running and REPLACED by the authoritative,
// corroborated, sorted list on completion. Stats appears when done.
type ScanJob struct {
	ID        string          `json:"id"`
	Path      string          `json:"path"`
	Status    ScanStatus      `json:"status"`
	Error     string          `json:"error,omitempty"`
	StartedAt string          `json:"started_at"`
	Progress  ScanProgress    `json:"progress"`
	Books     []ScannedBook   `json:"books"`
	Stats     *metascan.Stats `json:"stats,omitempty"`
}

// ScanJobSummary is a ScanJob without its (potentially large) book list - the
// list-endpoint shape used to reattach to running/recent scans after a reload.
type ScanJobSummary struct {
	ID        string          `json:"id"`
	Path      string          `json:"path"`
	Status    ScanStatus      `json:"status"`
	Error     string          `json:"error,omitempty"`
	StartedAt string          `json:"started_at"`
	Progress  ScanProgress    `json:"progress"`
	Stats     *metascan.Stats `json:"stats,omitempty"`
}

// bookIdent is a book's coverage-resolution input. workID (a manual override)
// takes precedence over the identity when set. fp is the precomputed fingerprint
// (see newBookIdent), so the hot snapshot/progress paths compare a stored string
// instead of rebuilding it per book on every ~700ms poll.
type bookIdent struct {
	id     BookIdentity
	workID string
	fp     string
}

// newBookIdent builds a bookIdent and precomputes its fingerprint: a stable
// string over the resolution inputs, so a worker's verdict is only applied to a
// book whose identity has not changed since dispatch (corroboration can rewrite
// a streamed book's series/title/position).
func newBookIdent(id BookIdentity, workID string) bookIdent {
	return bookIdent{
		id:     id,
		workID: workID,
		fp: strings.Join([]string{
			id.ASIN, id.ISBN, id.Title, id.Series, id.SeriesPos,
			strings.Join(id.Authors, ","), workID,
		}, "\x00"),
	}
}

// resolvedCov is a completed coverage verdict tagged with the fingerprint it was
// resolved for.
type resolvedCov struct {
	fingerprint string
	coverage    Coverage
}

// scanJob is the mutable, in-memory job state (guarded by ScanManager.mu). The
// books slice grows during the scan; coverage lives in resolved (keyed by path)
// and is overlaid onto books at snapshot time, decoupling worker output from the
// slice's shifting identity across the mid-scan replace.
type scanJob struct {
	id        string
	seq       int64
	path      string
	status    ScanStatus
	errMsg    string
	startedAt time.Time
	phase     string

	groupsDone  int
	groupsTotal int

	books      []ScannedBook          // identity + hidden (coverage filled at snapshot)
	idents     map[string]bookIdent   // path -> current resolution input
	dispatched map[string]string      // path -> fingerprint already dispatched
	resolved   map[string]resolvedCov // path -> completed verdict

	stats *metascan.Stats
	wg    sync.WaitGroup // coverage workers
	sem   chan struct{}  // coverage concurrency bound
}

// overridePatch is a live (this-session) override applied at read time to every
// job's matching book, so a hide/manual-match issued after a scan completed still
// reflects on the next poll without re-running coverage.
type overridePatch struct {
	hidden   bool
	coverage *Coverage // non-nil for a manual match
}

// ScanManager runs and tracks folder-scan jobs. Results are held in memory (a
// large folder can take a while due to ffprobe), keyed by job id.
type ScanManager struct {
	ctx         context.Context //nolint:containedctx // daemon-lifetime ctx for background jobs
	client      *Client
	ffprobePath string
	overrides   OverrideLookup // may be nil (no persisted overrides)
	scan        scanFunc       // defaults to metascan.Scan

	mu      sync.Mutex
	seq     int64
	jobs    map[string]*scanJob
	patches map[string]overridePatch // path -> live override patch
}

// NewScanManager returns a manager whose background jobs live under ctx (the
// daemon lifetime). client resolves coverage; ffprobePath enriches runtimes
// ("" disables enrichment); overrides supplies persisted hide/manual-match state
// (nil = none).
func NewScanManager(ctx context.Context, client *Client, ffprobePath string, overrides OverrideLookup) *ScanManager {
	return &ScanManager{
		ctx:         ctx,
		client:      client,
		ffprobePath: ffprobePath,
		overrides:   overrides,
		scan:        metascan.Scan,
		jobs:        map[string]*scanJob{},
		patches:     map[string]overridePatch{},
	}
}

// newJobID returns a random hex job id.
func newJobID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Start validates that path is an existing directory, registers a running job,
// and kicks off the scan in the background. It returns the job id.
func (m *ScanManager) Start(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("scan path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("scan path %q is not a directory", path)
	}
	// Canonicalize the root once (abs + symlink-eval + clean) so every SourcePath
	// (filepath.Join(job.path, rel)) and the displayed root are stable regardless
	// of the spelling used (trailing slash, relative path, symlink) - a stored
	// override keys on the same canonical form, so the two always match.
	if canon, err := resolvePath(path); err == nil {
		path = canon
	}
	id := newJobID()
	m.mu.Lock()
	m.seq++
	m.jobs[id] = &scanJob{
		id:         id,
		seq:        m.seq,
		path:       path,
		status:     ScanRunning,
		startedAt:  time.Now().UTC(),
		phase:      "scanning",
		idents:     map[string]bookIdent{},
		dispatched: map[string]string{},
		resolved:   map[string]resolvedCov{},
		sem:        make(chan struct{}, coverageWorkers),
	}
	m.pruneLocked()
	m.mu.Unlock()
	go m.run(id, path)
	return id, nil
}

// Get returns a serialize-safe snapshot copy of the job, or (nil, false).
func (m *ScanManager) Get(id string) (ScanJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return ScanJob{}, false
	}
	return m.snapshotLocked(job), true
}

// List returns job summaries (no book lists) newest-first, for a reloaded UI to
// reattach to running and recent scans. It derives only the counters - no book
// deep-copies - so listing stays cheap with large retained scans.
func (m *ScanManager) List() []ScanJobSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs := make([]*scanJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].seq > jobs[j].seq })
	out := make([]ScanJobSummary, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, ScanJobSummary{
			ID: j.id, Path: j.path, Status: j.status, Error: j.errMsg,
			StartedAt: j.startedAt.Format(time.RFC3339),
			Progress:  m.progressLocked(j), Stats: j.stats,
		})
	}
	return out
}

// ApplyOverride reflects a just-persisted override on the in-memory jobs at read
// time: matching books (by source path) show the new hidden flag and, for a
// manual match, the resolved coverage. Clearing an override (hidden=false and
// cov=nil) drops the patch. Cheap - it stores one map entry consulted per book at
// snapshot time - so a completed job reflects the change on the next poll.
func (m *ScanManager) ApplyOverride(sourcePath string, hidden bool, cov *Coverage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !hidden && cov == nil {
		delete(m.patches, sourcePath)
		return
	}
	m.patches[sourcePath] = overridePatch{hidden: hidden, coverage: cov}
}

// pruneLocked drops finished jobs beyond retainedFinished (newest kept), keeping
// every still-running job. Caller holds mu.
func (m *ScanManager) pruneLocked() {
	finished := make([]*scanJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		if j.status != ScanRunning {
			finished = append(finished, j)
		}
	}
	if len(finished) <= retainedFinished {
		return
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].seq > finished[j].seq })
	for _, j := range finished[retainedFinished:] {
		delete(m.jobs, j.id)
	}
}

// loadOverrides fetches the persisted override set (empty on nil lookup or error -
// overrides are best-effort and never fail a scan).
func (m *ScanManager) loadOverrides() map[string]Override {
	if m.overrides == nil {
		return map[string]Override{}
	}
	ov, err := m.overrides(m.ctx)
	if err != nil || ov == nil {
		return map[string]Override{}
	}
	return ov
}

// run executes the scan, streaming books and resolving coverage as they arrive,
// then replaces the list with the authoritative result and waits for coverage to
// settle. It never panics the daemon: a scan error is recorded on the job.
func (m *ScanManager) run(id, path string) {
	overrides := m.loadOverrides()

	res, stats, err := m.scan(path, metascan.Options{
		FFprobePath: m.ffprobePath,
		OnProgress: func(done, total int) {
			m.mu.Lock()
			if job, ok := m.jobs[id]; ok {
				job.groupsDone, job.groupsTotal = done, total
			}
			m.mu.Unlock()
		},
		OnBook: func(b metascan.Book) { m.streamBook(id, b, overrides) },
	})
	if err != nil {
		m.finishError(id, err.Error())
		return
	}

	// Replace the provisional list with the authoritative one and re-resolve
	// coverage through the caches (only corrected-identity books re-hit the
	// network), then wait for all coverage workers before marking done.
	m.applyFinal(id, res, overrides)
	m.waitCoverage(id)
	m.finishDone(id, stats)
}

// streamBook appends a provisional book and dispatches its coverage resolution.
func (m *ScanManager) streamBook(id string, b metascan.Book, overrides map[string]Override) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return
	}
	sb, bi := convertBook(b, job.path, overrides)
	job.books = append(job.books, sb)
	job.idents[sb.Path] = bi
	m.ensureResolvedLocked(job, sb.Path)
}

// applyFinal swaps in the authoritative (corroborated, sorted) book list and
// (re)dispatches coverage for each, then moves the phase to "coverage".
func (m *ScanManager) applyFinal(id string, res *metascan.Result, overrides map[string]Override) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return
	}
	books := make([]ScannedBook, 0, len(res.Books))
	for _, b := range res.Books {
		sb, bi := convertBook(b, job.path, overrides)
		books = append(books, sb)
		job.idents[sb.Path] = bi
	}
	job.books = books
	job.phase = "coverage"
	for _, sb := range books {
		m.ensureResolvedLocked(job, sb.Path)
	}
}

// ensureResolvedLocked dispatches a coverage worker for a book's current identity
// if one has not already been dispatched for that exact identity. Caller holds mu.
func (m *ScanManager) ensureResolvedLocked(job *scanJob, path string) {
	bi := job.idents[path]
	fp := bi.fp
	if job.dispatched[path] == fp {
		return
	}
	job.dispatched[path] = fp
	job.wg.Add(1)
	go m.resolveWorker(job, path, fp, bi)
}

// resolveWorker resolves one book's coverage (bounded by the job semaphore) and
// records the verdict against its fingerprint. Coverage never fails the scan: a
// down/absent service or a stale manual id degrades to unavailable.
func (m *ScanManager) resolveWorker(job *scanJob, path, fp string, bi bookIdent) {
	defer job.wg.Done()
	job.sem <- struct{}{}
	defer func() { <-job.sem }()

	var cov Coverage
	if bi.workID != "" {
		if c, err := m.client.CoverageForWork(m.ctx, bi.workID); err == nil {
			cov = c
		}
	} else if c, err := m.client.CoverageFor(m.ctx, bi.id); err == nil {
		cov = c
	}

	m.mu.Lock()
	// Only record the verdict if the book's identity has NOT changed since dispatch.
	// Corroboration can re-identify a book (new fp + a fresh dispatch) between the
	// streaming pass and applyFinal; a stale worker finishing last must not clobber
	// the fresh verdict - the fp-gated readers would then never apply it and, since
	// dispatched[path] already carries the new fp, never re-dispatch (coverage
	// wedges and coverage_done never reaches the total). Dropping the stale write
	// (including the path-no-longer-present case) keeps resolved consistent.
	if bi, ok := job.idents[path]; ok && bi.fp == fp {
		job.resolved[path] = resolvedCov{fingerprint: fp, coverage: cov}
	}
	m.mu.Unlock()
}

// waitCoverage blocks until every dispatched coverage worker for the job has
// finished (all dispatches are complete once the scan returned and applyFinal ran).
func (m *ScanManager) waitCoverage(id string) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	m.mu.Unlock()
	if ok {
		job.wg.Wait()
	}
}

func (m *ScanManager) finishError(id, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job, ok := m.jobs[id]; ok {
		job.status = ScanError
		job.errMsg = msg
		job.phase = "done"
	}
	m.pruneLocked()
}

func (m *ScanManager) finishDone(id string, stats metascan.Stats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job, ok := m.jobs[id]; ok {
		job.status = ScanDone
		job.phase = "done"
		st := stats
		job.stats = &st
	}
	m.pruneLocked()
}

// snapshotLocked builds a serialize-safe ScanJob: it deep-copies the book list,
// overlays each book's resolved coverage (only when the resolution matches the
// book's current identity) and any live override patch, and derives the counters.
// Caller holds mu.
func (m *ScanManager) snapshotLocked(job *scanJob) ScanJob {
	books := make([]ScannedBook, len(job.books))
	for i, sb := range job.books {
		b := sb
		if len(b.Authors) > 0 {
			b.Authors = append([]string(nil), b.Authors...)
		}
		if len(b.Sources) > 0 {
			b.Sources = maps.Clone(b.Sources)
		}
		if rc, ok := job.resolved[b.Path]; ok && rc.fingerprint == job.idents[b.Path].fp {
			b.Coverage = rc.coverage
		}
		if p, ok := m.patches[b.SourcePath]; ok {
			b.Hidden = p.hidden
			if p.coverage != nil {
				b.Coverage = *p.coverage
			}
		}
		books[i] = b
	}
	return ScanJob{
		ID:        job.id,
		Path:      job.path,
		Status:    job.status,
		Error:     job.errMsg,
		StartedAt: job.startedAt.Format(time.RFC3339),
		Progress:  m.progressLocked(job),
		Books:     books,
		Stats:     job.stats,
	}
}

// progressLocked derives a job's counters without copying its books, so the
// list endpoint (and each snapshot) stays cheap on large scans. Caller holds mu.
func (m *ScanManager) progressLocked(job *scanJob) ScanProgress {
	coverageDone := 0
	for _, sb := range job.books {
		if rc, ok := job.resolved[sb.Path]; ok && rc.fingerprint == job.idents[sb.Path].fp {
			coverageDone++
		}
	}
	return ScanProgress{
		Phase:         job.phase,
		GroupsDone:    job.groupsDone,
		GroupsTotal:   job.groupsTotal,
		BooksFound:    len(job.books),
		CoverageDone:  coverageDone,
		CoverageTotal: len(job.books),
	}
}

// convertBook maps a metascan.Book to a ScannedBook (applying the hide override)
// and its coverage-resolution input (folding in a manual work override). root is
// the scan root: metascan paths are root-relative, but overrides - like every
// durable source_path - key on the absolute folder.
func convertBook(b metascan.Book, root string, overrides map[string]Override) (ScannedBook, bookIdent) {
	sb := ScannedBook{
		Path: b.Path, SourcePath: filepath.Join(root, filepath.FromSlash(b.Path)),
		Title: b.Title, Subtitle: b.Subtitle, Authors: b.Authors,
		Narrators: b.Narrators, Series: b.Series, SeriesPosition: b.SeriesPosition,
		ASIN: b.ASIN, ISBN: b.ISBN, RuntimeMin: b.RuntimeMin, Chapters: b.Chapters,
		AudioFiles: b.AudioFiles, Sources: b.Sources,
	}
	id := BookIdentity{
		ASIN: b.ASIN, ISBN: b.ISBN, Title: b.Title,
		Authors: b.Authors, Series: b.Series, SeriesPos: b.SeriesPosition,
	}
	workID := ""
	if ov, ok := overrides[sb.SourcePath]; ok {
		sb.Hidden = ov.Hidden
		workID = ov.WorkID
	}
	return sb, newBookIdent(id, workID)
}
