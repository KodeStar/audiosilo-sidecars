package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// requirePipeline wraps a pipeline handler so it 503s when the pipeline
// dependencies are not wired (tests that only cover the M0 auth/settings surface
// leave them nil). Composed at route registration alongside requireAuth, so no
// handler repeats the guard.
func (a *API) requirePipeline(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.store == nil || a.sched == nil || a.scans == nil {
			writeError(w, http.StatusServiceUnavailable, "pipeline not available")
			return
		}
		next(w, r)
	}
}

// --- scans ---

type createScanRequest struct {
	Path string `json:"path"`
}

type createScanResponse struct {
	JobID string `json:"job_id"`
}

func (a *API) handleCreateScan(w http.ResponseWriter, r *http.Request) {
	var req createScanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	// Enforce the library_roots allow-list (empty list = allow any local path).
	roots := a.snapshot().LibraryRoots
	ok, err := metaops.PathAllowed(path, roots)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "path is outside the configured library roots")
		return
	}
	jobID, err := a.scans.Start(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, createScanResponse{JobID: jobID})
}

func (a *API) handleGetScan(w http.ResponseWriter, r *http.Request) {
	job, ok := a.scans.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "scan not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

type listScansResponse struct {
	Scans []metaops.ScanJobSummary `json:"scans"`
}

// handleListScans returns the running + recent scan jobs (newest first, no book
// lists) so a reloaded UI can reattach to in-flight and just-finished scans.
func (a *API) handleListScans(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, listScansResponse{Scans: a.scans.List()})
}

// --- books ---

// bookCandidate is one selected book to enqueue. Coverage and Sources are the
// advisory scan-time snapshot the Library UI already holds (the metadata-coverage
// verdict and the per-field provenance); they are persisted as-is and echoed back
// on the book view.
type bookCandidate struct {
	SourcePath string            `json:"source_path"`
	Title      string            `json:"title"`
	Authors    []string          `json:"authors"`
	Series     string            `json:"series"`
	SeriesPos  string            `json:"series_pos"`
	ASIN       string            `json:"asin"`
	ISBN       string            `json:"isbn"`
	WorkID     string            `json:"work_id,omitempty"`
	Coverage   json.RawMessage   `json:"coverage,omitempty"`
	Sources    map[string]string `json:"sources,omitempty"`
}

type createBooksRequest struct {
	Candidates []bookCandidate `json:"candidates"`
}

// bookCreateResult is the per-candidate outcome (created book or a conflict).
type bookCreateResult struct {
	SourcePath string    `json:"source_path"`
	Created    bool      `json:"created"`
	Conflict   bool      `json:"conflict,omitempty"`
	Error      string    `json:"error,omitempty"`
	Book       *bookView `json:"book,omitempty"`
}

type createBooksResponse struct {
	Results []bookCreateResult `json:"results"`
}

func (a *API) handleCreateBooks(w http.ResponseWriter, r *http.Request) {
	var req createBooksRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Candidates) == 0 {
		writeError(w, http.StatusBadRequest, "candidates is required")
		return
	}
	ctx := r.Context()
	// Enforce the same library_roots allow-list scans use (empty list = allow any
	// local path), so a book can only be enqueued from a permitted location.
	roots := a.snapshot().LibraryRoots
	results := make([]bookCreateResult, 0, len(req.Candidates))
	created := 0
	for _, c := range req.Candidates {
		sp := strings.TrimSpace(c.SourcePath)
		res := bookCreateResult{SourcePath: sp}
		if sp == "" || strings.TrimSpace(c.Title) == "" {
			res.Error = "source_path and title are required"
			results = append(results, res)
			continue
		}
		if ok, perr := metaops.PathAllowed(sp, roots); perr != nil || !ok {
			res.Error = "path not allowed"
			results = append(results, res)
			continue
		}
		sources := c.Sources
		if sources == nil {
			sources = map[string]string{}
		}
		nb := store.NewBook{
			SourcePath:      sp,
			WorkDir:         store.DeriveWorkDir(a.workRoot(), sp, c.Title),
			Title:           strings.TrimSpace(c.Title),
			Authors:         c.Authors,
			Series:          strings.TrimSpace(c.Series),
			SeriesPos:       strings.TrimSpace(c.SeriesPos),
			ASIN:            strings.TrimSpace(c.ASIN),
			ISBN:            strings.TrimSpace(c.ISBN),
			IdentitySources: sources,
			WorkID:          strings.TrimSpace(c.WorkID),
			Coverage:        c.Coverage,
		}
		b, err := a.store.CreateBook(ctx, nb)
		switch {
		case errors.Is(err, store.ErrDuplicate):
			res.Conflict = true
			res.Error = "already enqueued"
		case err != nil:
			res.Error = "could not create book"
		default:
			res.Created = true
			v := a.bookToView(ctx, b)
			res.Book = &v
			created++
		}
		results = append(results, res)
	}
	if created > 0 {
		a.sched.Notify()
	}
	writeJSON(w, http.StatusOK, createBooksResponse{Results: results})
}

// workRoot is the daemon's per-book scratch-dir root (<data>/work). The
// slug/hash derivation itself lives in internal/store (DeriveWorkDir) so it is
// unit-testable without the transport layer.
func (a *API) workRoot() string {
	return filepath.Join(a.dataDir, "work")
}

// bookView is the API shape of a book, with live progress merged in. Lane is the
// served lane the current state runs in (state.LaneOf), so the web UI need not
// mirror the state->lane table.
type bookView struct {
	ID              int64             `json:"id"`
	SourcePath      string            `json:"source_path"`
	Title           string            `json:"title"`
	Authors         []string          `json:"authors"`
	Series          string            `json:"series,omitempty"`
	SeriesPos       string            `json:"series_pos,omitempty"`
	ASIN            string            `json:"asin,omitempty"`
	ISBN            string            `json:"isbn,omitempty"`
	IdentitySources map[string]string `json:"identity_sources"`
	WorkID          string            `json:"work_id,omitempty"`
	State           string            `json:"state"`
	Lane            string            `json:"lane"`
	Status          string            `json:"status"`
	Error           string            `json:"error,omitempty"`
	Coverage        json.RawMessage   `json:"coverage,omitempty"`
	Progress        []store.Progress  `json:"progress"`
	// ScratchBytes is the current on-disk size of the book's work dir (chapters +
	// durables), so the UI can show disk usage and offer a purge. 0 when the work
	// dir does not exist yet (or was purged).
	ScratchBytes int64 `json:"scratch_bytes"`
	// TotalCostUSD is the summed agent spend across the book's stage runs (0 for a
	// book that has run only mechanical/ASR stages or none yet), attached on both the
	// list and detail views so the Running/Done UI can show a per-book cost.
	TotalCostUSD float64 `json:"total_cost_usd"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// bookDetail adds the per-execution stage-run ledger.
type bookDetail struct {
	bookView
	StageRuns []store.StageRun `json:"stage_runs"`
}

// bookToView builds the detail view for one book, fetching its progress and summed
// agent cost. The list endpoint uses buildBookView with pre-fetched progress + totals
// to avoid an N+1.
func (a *API) bookToView(ctx context.Context, b store.Book) bookView {
	progress, _ := a.store.ListProgress(ctx, b.ID)
	totalCost, _ := a.store.SumStageRunCost(ctx, b.ID)
	return buildBookView(b, progress, totalCost)
}

// buildBookView assembles a bookView from a book, its (possibly nil) progress rows, and
// its summed agent cost, normalizing the always-present JSON fields. scratch_bytes is
// served from the persisted column (written by the split stage / PurgeScratch), so no
// read walks the work dir.
func buildBookView(b store.Book, progress []store.Progress, totalCostUSD float64) bookView {
	authors := b.Authors
	if authors == nil {
		authors = []string{}
	}
	idsrc := b.IdentitySources
	if idsrc == nil {
		idsrc = map[string]string{}
	}
	if progress == nil {
		progress = []store.Progress{}
	}
	return bookView{
		ID: b.ID, SourcePath: b.SourcePath, Title: b.Title, Authors: authors,
		Series: b.Series, SeriesPos: b.SeriesPos, ASIN: b.ASIN, ISBN: b.ISBN,
		IdentitySources: idsrc, WorkID: b.WorkID,
		State: b.State, Lane: string(state.LaneOf(state.State(b.State))),
		Status: b.Status, Error: b.Error, Coverage: b.Coverage,
		Progress: progress, ScratchBytes: b.ScratchBytes, TotalCostUSD: totalCostUSD,
		CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
	}
}

type listBooksResponse struct {
	Books []bookView `json:"books"`
}

func (a *API) handleListBooks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	books, err := a.store.ListBooks(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list books")
		return
	}
	// One grouped progress query for the whole list instead of one per book.
	progressByBook, err := a.store.ListAllProgress(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list progress")
		return
	}
	// One grouped cost-rollup query for the whole list (no N+1).
	costByBook, err := a.store.StageRunTotals(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list costs")
		return
	}
	views := make([]bookView, 0, len(books))
	for _, b := range books {
		views = append(views, buildBookView(b, progressByBook[b.ID], costByBook[b.ID]))
	}
	writeJSON(w, http.StatusOK, listBooksResponse{Books: views})
}

func (a *API) handleGetBook(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	b, err := a.store.GetBook(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read book")
		return
	}
	runs, err := a.store.ListStageRuns(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read stage runs")
		return
	}
	if runs == nil {
		runs = []store.StageRun{}
	}
	writeJSON(w, http.StatusOK, bookDetail{bookView: a.bookToView(ctx, b), StageRuns: runs})
}

// bookAction adapts a scheduler control method to a handler, mapping its errors
// to status codes uniformly (pause/resume/retry/cancel share this shape).
func (a *API) bookAction(fn func(*scheduler.Scheduler, context.Context, int64) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		err := fn(a.sched, r.Context(), id)
		writeControlResult(w, err)
	}
}

func (a *API) handleDeleteBook(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	writeControlResult(w, a.sched.Delete(r.Context(), id))
}

// handlePurgeScratch reclaims a book's split chapters (the M2 heavy scratch). The
// scheduler enforces the allowed states (done/paused/failed) and the not-busy
// guard, mapped to 409 by writeControlResult.
func (a *API) handlePurgeScratch(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	writeControlResult(w, a.sched.PurgeScratch(r.Context(), id))
}

// writeControlResult maps a scheduler control error to an HTTP status.
func writeControlResult(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "book not found")
	case errors.Is(err, scheduler.ErrInvalidOp):
		writeError(w, http.StatusConflict, "operation not valid for the book's current status")
	case errors.Is(err, scheduler.ErrBusy):
		writeError(w, http.StatusConflict, "book is running a stage; cancel or pause it first")
	default:
		writeError(w, http.StatusInternalServerError, "operation failed")
	}
}

// parseID reads the {id} path value as an int64.
func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid book id")
		return 0, false
	}
	return id, true
}
