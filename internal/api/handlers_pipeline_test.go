package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/auth"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/pipeline"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// pipelineEnv wires the full pipeline surface over an in-memory store. The
// scheduler is NOT Started, so books sit in their initial state and control
// endpoints act on stable rows (deterministic tests).
type pipelineEnv struct {
	*testEnv
	db  *store.DB
	cfg config.Config
}

func newPipelineEnv(t *testing.T, libraryRoots []string, opts ...func(*Deps)) *pipelineEnv {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mgr := auth.New(db.AuthStore())
	pw, err := mgr.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	cfg := config.Default()
	cfg.LibraryRoots = libraryRoots
	cfg.Metadata.BaseURL = "" // disabled: coverage is 'unavailable' in tests

	hub := events.NewHub(64)
	sched := scheduler.New(db, hub, scheduler.NewStubExecutor(0, 0), 2, t.TempDir(), false)
	meta := metaops.NewClient(cfg.Metadata.BaseURL)
	scans := metaops.NewScanManager(context.Background(), meta, "", storeOverrides(db))

	env := &testEnv{password: pw}
	deps := Deps{
		Auth:      mgr,
		Limiter:   auth.NewRateLimiter(100, 100),
		Secrets:   secrets.NewMemStore(),
		Events:    hub,
		Version:   "test",
		DataDir:   t.TempDir(),
		Store:     db,
		Scheduler: sched,
		Scans:     scans,
		Meta:      meta,
		Config:    cfg,
		// SidecarLoader mirrors the server.go wiring: the real pipeline compose/flatten
		// adapted to JSON, translating pipeline's no-sidecars sentinel to the api's so
		// the handler maps it to 404.
		SidecarLoader: func(workDir string) (json.RawMessage, error) {
			raw, err := pipeline.SidecarsViewJSON(workDir)
			if errors.Is(err, pipeline.ErrNoSidecars) {
				return nil, ErrNoSidecars
			}
			return raw, err
		},
		// The two pipeline loaders mirror server.go; the Contrib service is wired per
		// test via opts (it needs a fake-GitHub base URL).
		CoreProposalLoader: func(workDir string) (json.RawMessage, error) {
			raw, err := pipeline.CoreProposalJSON(workDir)
			if errors.Is(err, pipeline.ErrNoCoreProposal) {
				return nil, ErrNoCoreProposal
			}
			return raw, err
		},
		ExportArchive: func(b store.Book) ([]byte, string, error) {
			slug := pipeline.ExportSlug(b)
			data, err := pipeline.ExportArchive(b.WorkDir, slug)
			if errors.Is(err, pipeline.ErrNoSidecars) {
				return nil, "", ErrNoSidecars
			}
			return data, slug + "-sidecars.zip", err
		},
	}
	for _, opt := range opts {
		opt(&deps)
	}
	env.api = New(deps)
	env.srv = httptest.NewServer(env.api.Handler())
	t.Cleanup(env.srv.Close)
	return &pipelineEnv{testEnv: env, db: db, cfg: cfg}
}

func TestScanPathAllowListAllowedAndDenied(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "audiobooks")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()

	env := newPipelineEnv(t, []string{root})
	token := env.login(t)

	// Allowed: inside a configured root -> 202 with a job id.
	resp := env.do(t, http.MethodPost, "/api/v1/scans", token, `{"path":"`+inside+`"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("allowed scan = %d, want 202", resp.StatusCode)
	}
	var cr createScanResponse
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	if cr.JobID == "" {
		t.Fatal("no job id returned")
	}

	// Denied: outside every root -> 403.
	resp = env.do(t, http.MethodPost, "/api/v1/scans", token, `{"path":"`+outside+`"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("outside-root scan = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing path -> 400.
	resp = env.do(t, http.MethodPost, "/api/v1/scans", token, `{"path":""}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty path = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Poll the allowed job to completion.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r := env.do(t, http.MethodGet, "/api/v1/scans/"+cr.JobID, token, "")
		var job metaops.ScanJob
		_ = json.NewDecoder(r.Body).Decode(&job)
		r.Body.Close()
		if job.Status == metaops.ScanDone {
			return
		}
		if job.Status == metaops.ScanError {
			t.Fatalf("scan errored: %s", job.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scan did not finish")
}

func TestScanUnknownJob(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)
	resp := env.do(t, http.MethodGet, "/api/v1/scans/nope", token, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown scan = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestCreateBooksPersistsNarrators asserts the POST /books candidate's narrators are
// stored via NewBook (they ride to the contributing stage's core proposal).
func TestCreateBooksPersistsNarrators(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)

	body := `{"candidates":[{"source_path":"/b/narr","title":"Narrated",` +
		`"authors":["Auth One"],"narrators":["Nora Narrator","Sam Speaker"]}]}`
	resp := env.do(t, http.MethodPost, "/api/v1/books", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create = %d, want 200", resp.StatusCode)
	}
	var cr createBooksResponse
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	if len(cr.Results) != 1 || cr.Results[0].Book == nil {
		t.Fatalf("results = %+v", cr.Results)
	}

	got, err := env.db.GetBook(context.Background(), cr.Results[0].Book.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if len(got.Narrators) != 2 || got.Narrators[0] != "Nora Narrator" || got.Narrators[1] != "Sam Speaker" {
		t.Errorf("persisted narrators = %+v, want [Nora Narrator, Sam Speaker]", got.Narrators)
	}
}

func TestBooksCreateListDedupAndDetail(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)

	body := `{"candidates":[
		{"source_path":"/b/a","title":"A One","series":"S1","series_pos":"1",
		 "coverage":{"available":true,"known":true},"sources":{"title":"tag","series":"path"}},
		{"source_path":"/b/b","title":"S1 Two","series":"S1","series_pos":"2"},
		{"source_path":"/b/c","title":"C Solo","series":"S2","series_pos":"1"}
	]}`
	resp := env.do(t, http.MethodPost, "/api/v1/books", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create books = %d, want 200", resp.StatusCode)
	}
	var cr createBooksResponse
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	if len(cr.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(cr.Results))
	}
	var firstID int64
	for _, r := range cr.Results {
		if !r.Created || r.Book == nil {
			t.Fatalf("candidate %s not created: %+v", r.SourcePath, r)
		}
		if r.Book.State != "queued" {
			t.Errorf("new book state = %q, want queued", r.Book.State)
		}
		// queued is a waypoint, so its served lane is empty.
		if r.Book.Lane != "" {
			t.Errorf("queued book lane = %q, want empty", r.Book.Lane)
		}
		if r.SourcePath == "/b/a" {
			firstID = r.Book.ID
			// The advisory coverage + provenance snapshot round-trips.
			if string(r.Book.Coverage) != `{"available":true,"known":true}` {
				t.Errorf("coverage not persisted: %s", r.Book.Coverage)
			}
			if r.Book.IdentitySources["title"] != "tag" || r.Book.IdentitySources["series"] != "path" {
				t.Errorf("identity_sources not persisted: %+v", r.Book.IdentitySources)
			}
		}
	}

	// Dedup: re-POST the same source_path -> conflict, not created.
	resp = env.do(t, http.MethodPost, "/api/v1/books", token, `{"candidates":[{"source_path":"/b/a","title":"A One"}]}`)
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	if len(cr.Results) != 1 || cr.Results[0].Created || !cr.Results[0].Conflict {
		t.Fatalf("dedup result = %+v", cr.Results)
	}

	// List shows 3 books.
	resp = env.do(t, http.MethodGet, "/api/v1/books", token, "")
	var lr listBooksResponse
	_ = json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if len(lr.Books) != 3 {
		t.Fatalf("list = %d, want 3", len(lr.Books))
	}

	// Detail includes a (possibly empty) stage-run ledger.
	resp = env.do(t, http.MethodGet, "/api/v1/books/"+strconv.FormatInt(firstID, 10), token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("book detail = %d, want 200", resp.StatusCode)
	}
	var detail bookDetail
	_ = json.NewDecoder(resp.Body).Decode(&detail)
	resp.Body.Close()
	if detail.ID != firstID || detail.StageRuns == nil {
		t.Fatalf("detail = %+v", detail)
	}

	// Missing book -> 404.
	resp = env.do(t, http.MethodGet, "/api/v1/books/99999", token, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing book = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBookControlEndpoints(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)
	resp := env.do(t, http.MethodPost, "/api/v1/books", token, `{"candidates":[{"source_path":"/b/x","title":"X"}]}`)
	var cr createBooksResponse
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	id := strconv.FormatInt(cr.Results[0].Book.ID, 10)

	// Pause -> 204, then GET shows paused.
	if r := env.do(t, http.MethodPost, "/api/v1/books/"+id+"/pause", token, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("pause = %d, want 204", r.StatusCode)
	} else {
		r.Body.Close()
	}
	r := env.do(t, http.MethodGet, "/api/v1/books/"+id, token, "")
	var d bookDetail
	_ = json.NewDecoder(r.Body).Decode(&d)
	r.Body.Close()
	if d.Status != "paused" {
		t.Fatalf("status = %q, want paused", d.Status)
	}

	// Resume -> 204; resuming again -> 409 (not paused any more).
	if r := env.do(t, http.MethodPost, "/api/v1/books/"+id+"/resume", token, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("resume = %d, want 204", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if r := env.do(t, http.MethodPost, "/api/v1/books/"+id+"/resume", token, ""); r.StatusCode != http.StatusConflict {
		t.Errorf("double resume = %d, want 409", r.StatusCode)
		r.Body.Close()
	} else {
		r.Body.Close()
	}

	// Retry on a non-failed book -> 409.
	if r := env.do(t, http.MethodPost, "/api/v1/books/"+id+"/retry", token, ""); r.StatusCode != http.StatusConflict {
		t.Errorf("retry non-failed = %d, want 409", r.StatusCode)
		r.Body.Close()
	} else {
		r.Body.Close()
	}

	// Delete -> 204; deleting again -> 404.
	if r := env.do(t, http.MethodDelete, "/api/v1/books/"+id, token, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if r := env.do(t, http.MethodDelete, "/api/v1/books/"+id, token, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("delete missing = %d, want 404", r.StatusCode)
		r.Body.Close()
	} else {
		r.Body.Close()
	}
}

// TestPurgeScratchEndpoint covers the HTTP contract: 409 for a book in a
// non-purgeable state, 204 once it is paused, and 404 for an unknown book. (The
// actual chapters/ deletion + confinement guard are covered in
// internal/scheduler and internal/scratch.)
func TestPurgeScratchEndpoint(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)
	resp := env.do(t, http.MethodPost, "/api/v1/books", token, `{"candidates":[{"source_path":"/b/purge","title":"Purge Me"}]}`)
	var cr createBooksResponse
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	// The create result carries the disk gauge field.
	if cr.Results[0].Book == nil {
		t.Fatal("no book in create result")
	}
	bookID := cr.Results[0].Book.ID
	id := strconv.FormatInt(bookID, 10)

	// The list serves scratch_bytes from the persisted column, not a work-dir walk:
	// account a size directly in the store and expect the API to echo it (the book's
	// work dir does not exist on disk, so a walk would report 0).
	if err := env.db.UpdateScratchBytes(context.Background(), bookID, 4096); err != nil {
		t.Fatal(err)
	}
	// Account an agent stage-run cost so the list's total_cost_usd rollup is non-zero.
	if _, err := env.db.StartStageRun(context.Background(), bookID, "fact_pass", 1); err != nil {
		t.Fatal(err)
	}
	if err := env.db.AddOpenStageRunUsage(context.Background(), bookID, "fact_pass", "opus", 100, 40, 0.0123); err != nil {
		t.Fatal(err)
	}
	{
		r := env.do(t, http.MethodGet, "/api/v1/books", token, "")
		var lr listBooksResponse
		_ = json.NewDecoder(r.Body).Decode(&lr)
		r.Body.Close()
		if len(lr.Books) != 1 || lr.Books[0].ScratchBytes != 4096 {
			t.Fatalf("list scratch_bytes = %+v, want the column value 4096 (no walk)", lr.Books)
		}
		if c := lr.Books[0].TotalCostUSD; c < 0.0122 || c > 0.0124 {
			t.Fatalf("list total_cost_usd = %v, want ~0.0123", c)
		}
	}

	// A queued book (running, non-terminal) is not purgeable -> 409.
	if r := env.do(t, http.MethodPost, "/api/v1/books/"+id+"/purge-scratch", token, ""); r.StatusCode != http.StatusConflict {
		t.Errorf("purge queued = %d, want 409", r.StatusCode)
		r.Body.Close()
	} else {
		r.Body.Close()
	}

	// Pause it, then purge is allowed -> 204.
	env.do(t, http.MethodPost, "/api/v1/books/"+id+"/pause", token, "").Body.Close()
	if r := env.do(t, http.MethodPost, "/api/v1/books/"+id+"/purge-scratch", token, ""); r.StatusCode != http.StatusNoContent {
		t.Errorf("purge paused = %d, want 204", r.StatusCode)
		r.Body.Close()
	} else {
		r.Body.Close()
	}

	// Unknown book -> 404.
	if r := env.do(t, http.MethodPost, "/api/v1/books/99999/purge-scratch", token, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("purge missing = %d, want 404", r.StatusCode)
		r.Body.Close()
	} else {
		r.Body.Close()
	}

	// The book view exposes scratch_bytes.
	list := env.do(t, http.MethodGet, "/api/v1/books", token, "")
	body, _ := io.ReadAll(list.Body)
	list.Body.Close()
	if !strings.Contains(string(body), `"scratch_bytes"`) {
		t.Error("book view is missing scratch_bytes")
	}
}

func TestCreateBooksEnforcesLibraryRoots(t *testing.T) {
	root := t.TempDir()
	env := newPipelineEnv(t, []string{root})
	token := env.login(t)

	// The paths are resolved (symlink-evaluated) by PathAllowed; create both so the
	// resolution is stable on platforms with a symlinked temp root (e.g. macOS).
	inside := filepath.Join(root, "book-a")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "book-b")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	body := `{"candidates":[
		{"source_path":"` + inside + `","title":"Inside"},
		{"source_path":"` + outside + `","title":"Outside"}
	]}`
	resp := env.do(t, http.MethodPost, "/api/v1/books", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create books = %d, want 200", resp.StatusCode)
	}
	var cr createBooksResponse
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()

	byPath := map[string]bookCreateResult{}
	for _, r := range cr.Results {
		byPath[r.SourcePath] = r
	}
	// Allowed: inside a root -> created.
	if !byPath[inside].Created {
		t.Errorf("inside-root book not created: %+v", byPath[inside])
	}
	// Denied: outside every root -> not created, per-item error, batch still 200.
	if byPath[outside].Created || byPath[outside].Error != "path not allowed" {
		t.Errorf("outside-root book should be denied: %+v", byPath[outside])
	}
}

func TestStageRunWireShapeSnakeCase(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)
	resp := env.do(t, http.MethodPost, "/api/v1/books", token, `{"candidates":[{"source_path":"/b/sr","title":"SR"}]}`)
	var cr createBooksResponse
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	id := cr.Results[0].Book.ID

	// Record a stage run directly in the store, then read the detail view.
	runID, err := env.db.StartStageRun(context.Background(), id, "asr", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.db.FinishStageRun(context.Background(), runID, true, nil); err != nil {
		t.Fatal(err)
	}

	r := env.do(t, http.MethodGet, "/api/v1/books/"+strconv.FormatInt(id, 10), token, "")
	body := readAll(t, r)
	for _, key := range []string{`"id"`, `"book_id"`, `"stage"`, `"attempt"`, `"started_at"`, `"finished_at"`, `"ok"`, `"metrics"`} {
		if !strings.Contains(body, key) {
			t.Errorf("stage-run JSON missing snake_case key %s: %s", key, body)
		}
	}
}

// TestBookViewETAParkStartedFields covers the M6 bookView additions: park_code and
// started_at appear when set (and are omitted otherwise), and eta_seconds is omitted
// when the scheduler has published no ETA snapshot.
func TestBookViewETAParkStartedFields(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)
	ctx := context.Background()

	resp := env.do(t, http.MethodPost, "/api/v1/books", token,
		`{"candidates":[{"source_path":"/b/f","title":"F"}]}`)
	var cr createBooksResponse
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	id := cr.Results[0].Book.ID

	// A freshly created book: no stage run, no park, no ETA snapshot -> all three
	// fields omitted (omitempty).
	fresh := readAll(t, env.do(t, http.MethodGet, "/api/v1/books/"+strconv.FormatInt(id, 10), token, ""))
	for _, key := range []string{`"started_at"`, `"park_code"`, `"eta_seconds"`} {
		if strings.Contains(fresh, key) {
			t.Errorf("fresh book JSON unexpectedly contains %s: %s", key, fresh)
		}
	}

	// Give it a stage-run start and a typed park; both now surface on the detail view.
	if _, err := env.db.StartStageRun(ctx, id, "inspecting", 1); err != nil {
		t.Fatal(err)
	}
	if err := env.db.SetBookStatus(ctx, id, "needs_attention", "agent down", "agent_unavailable"); err != nil {
		t.Fatal(err)
	}
	detailBody := readAll(t, env.do(t, http.MethodGet, "/api/v1/books/"+strconv.FormatInt(id, 10), token, ""))
	if !strings.Contains(detailBody, `"started_at"`) {
		t.Errorf("detail JSON missing started_at: %s", detailBody)
	}
	if !strings.Contains(detailBody, `"park_code":"agent_unavailable"`) {
		t.Errorf("detail JSON missing park_code: %s", detailBody)
	}
	// eta_seconds stays omitted: the (unstarted) scheduler published no snapshot.
	if strings.Contains(detailBody, `"eta_seconds"`) {
		t.Errorf("eta_seconds present without a scheduler snapshot: %s", detailBody)
	}

	// The list view carries the same started_at + park_code (grouped queries).
	listBody := readAll(t, env.do(t, http.MethodGet, "/api/v1/books", token, ""))
	if !strings.Contains(listBody, `"started_at"`) || !strings.Contains(listBody, `"park_code":"agent_unavailable"`) {
		t.Errorf("list JSON missing started_at/park_code: %s", listBody)
	}
}

func TestPipelineEndpointsRequireAuthAndWiring(t *testing.T) {
	// Denied: no token on a pipeline endpoint.
	env := newPipelineEnv(t, nil)
	resp := env.do(t, http.MethodGet, "/api/v1/books", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token /books = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Unwired pipeline (M0-only env) -> 503.
	m0 := newTestEnv(t)
	token := m0.login(t)
	resp = m0.do(t, http.MethodGet, "/api/v1/books", token, "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unwired /books = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
}
