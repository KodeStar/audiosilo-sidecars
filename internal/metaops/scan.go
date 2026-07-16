package metaops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"

	metascan "github.com/kodestar/audiosilo-meta/pkg/scan"
)

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

// ScannedBook is one detected book plus its merged coverage verdict. The scan
// fields are hand-mirrored from metascan.Book (the meta module contract) so the
// API DTO is stable independent of that struct's private helpers.
type ScannedBook struct {
	Path           string   `json:"path"`
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
	// Sources records where each field came from ("tag" | "path" | "filename").
	Sources map[string]string `json:"sources,omitempty"`
	// Coverage is the metadata verdict (Available/Known/HasCharacters/HasRecaps).
	Coverage Coverage `json:"coverage"`
}

// ScanResult is a completed scan's book list.
type ScanResult struct {
	Root  string        `json:"root"`
	Books []ScannedBook `json:"books"`
}

// ScanProgress is the coarse progress of a scan job. During folder walking Total
// is 0; during the coverage merge Total is the book count and Done advances.
type ScanProgress struct {
	Phase string `json:"phase"` // "scanning" | "coverage" | "done"
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// ScanJob is an async folder-scan job (snapshot-safe; returned by copy).
type ScanJob struct {
	ID       string       `json:"id"`
	Path     string       `json:"path"`
	Status   ScanStatus   `json:"status"`
	Error    string       `json:"error,omitempty"`
	Progress ScanProgress `json:"progress"`
	Result   *ScanResult  `json:"result,omitempty"`
}

// ScanManager runs and tracks folder-scan jobs. Results are held in memory (a
// large folder can take a while due to ffprobe), keyed by job id.
type ScanManager struct {
	ctx         context.Context //nolint:containedctx // daemon-lifetime ctx for background jobs
	client      *Client
	ffprobePath string

	mu   sync.Mutex
	jobs map[string]*ScanJob
}

// NewScanManager returns a manager whose background jobs live under ctx (the
// daemon lifetime). client resolves coverage; ffprobePath enriches runtimes
// ("" disables enrichment).
func NewScanManager(ctx context.Context, client *Client, ffprobePath string) *ScanManager {
	return &ScanManager{ctx: ctx, client: client, ffprobePath: ffprobePath, jobs: map[string]*ScanJob{}}
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
	id := newJobID()
	job := &ScanJob{ID: id, Path: path, Status: ScanRunning, Progress: ScanProgress{Phase: "scanning"}}
	m.mu.Lock()
	m.jobs[id] = job
	m.mu.Unlock()
	go m.run(id, path)
	return id, nil
}

// Get returns a snapshot copy of the job (safe to serialize), or (nil, false).
func (m *ScanManager) Get(id string) (ScanJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return ScanJob{}, false
	}
	return *job, true // ScanJob holds only value fields + a *ScanResult snapshot
}

// run executes the scan then merges per-book coverage, updating the job as it
// goes. It never panics the daemon: a scan error is recorded on the job.
func (m *ScanManager) run(id, path string) {
	res, _, err := metascan.Scan(path, metascan.Options{FFprobePath: m.ffprobePath})
	if err != nil {
		m.finishError(id, err.Error())
		return
	}

	books := make([]ScannedBook, len(res.Books))
	m.setProgress(id, ScanProgress{Phase: "coverage", Done: 0, Total: len(res.Books)})
	for i, b := range res.Books {
		sb := ScannedBook{
			Path: b.Path, Title: b.Title, Subtitle: b.Subtitle, Authors: b.Authors,
			Narrators: b.Narrators, Series: b.Series, SeriesPosition: b.SeriesPosition,
			ASIN: b.ASIN, ISBN: b.ISBN, RuntimeMin: b.RuntimeMin, Chapters: b.Chapters,
			AudioFiles: b.AudioFiles, Sources: b.Sources,
		}
		// Coverage never fails the scan: a down/absent service marks it unavailable.
		cov, cerr := m.client.CoverageFor(m.ctx, b.ASIN, b.ISBN)
		if cerr == nil {
			sb.Coverage = cov
		}
		books[i] = sb
		m.setProgress(id, ScanProgress{Phase: "coverage", Done: i + 1, Total: len(res.Books)})
	}

	m.finishDone(id, &ScanResult{Root: res.Root, Books: books})
}

func (m *ScanManager) setProgress(id string, p ScanProgress) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job, ok := m.jobs[id]; ok {
		job.Progress = p
	}
}

func (m *ScanManager) finishError(id, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job, ok := m.jobs[id]; ok {
		job.Status = ScanError
		job.Error = msg
	}
}

func (m *ScanManager) finishDone(id string, result *ScanResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job, ok := m.jobs[id]; ok {
		job.Status = ScanDone
		job.Progress = ScanProgress{Phase: "done", Done: len(result.Books), Total: len(result.Books)}
		job.Result = result
	}
}
