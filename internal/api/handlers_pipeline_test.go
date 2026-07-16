package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/auth"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
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

func newPipelineEnv(t *testing.T, libraryRoots []string) *pipelineEnv {
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
	sched := scheduler.New(db, hub, scheduler.NewStubExecutor(0, 0), 2)
	scans := metaops.NewScanManager(context.Background(), metaops.NewClient(""), "")

	env := &testEnv{password: pw}
	env.api = New(Deps{
		Auth:      mgr,
		Limiter:   auth.NewRateLimiter(100, 100),
		Secrets:   secrets.NewMemStore(),
		Events:    hub,
		Version:   "test",
		DataDir:   t.TempDir(),
		Store:     db,
		Scheduler: sched,
		Scans:     scans,
		Config:    cfg,
	})
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

func TestBooksCreateListDedupAndDetail(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)

	body := `{"candidates":[
		{"source_path":"/b/a","title":"A One","series":"S1","series_pos":"1"},
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
		if r.SourcePath == "/b/a" {
			firstID = r.Book.ID
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
	resp = env.do(t, http.MethodGet, "/api/v1/books/"+itoa(firstID), token, "")
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
	id := itoa(cr.Results[0].Book.ID)

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

// itoa is a tiny int64->string helper (avoids importing strconv per test).
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
