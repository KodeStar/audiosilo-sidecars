package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/auth"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// storeOverrides adapts the store to metaops' store-agnostic OverrideLookup (also
// used by the pipeline test env). Kept in the test package to mirror server.go's
// production adapter.
func storeOverrides(db *store.DB) metaops.OverrideLookup {
	return func(ctx context.Context) (map[string]metaops.Override, error) {
		rows, err := db.ListOverrides(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]metaops.Override, len(rows))
		for _, o := range rows {
			out[o.SourcePath] = metaops.Override{Hidden: o.Hidden, WorkID: o.WorkID, WorkTitle: o.WorkTitle}
		}
		return out, nil
	}
}

// fakeMeta is a minimal meta.audiosilo.app for the override/search handlers: it
// resolves a fixed set of work ids and answers search with a fixed work hit.
func fakeMeta(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/works/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/api/v1/works/"):]
		if id != "w-good" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":"w-good","title":"Good Work","characters":[{"id":"x","name":"X"}]}`))
	})
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[` +
			`{"kind":"work","id":"w-good","title":"Good Work","authors":[{"id":"p","name":"An Author"}],"series":null,"cover_url":"http://x/c.jpg"},` +
			`{"kind":"person","id":"p1","name":"An Author"}` +
			`]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newMetaEnv wires the pipeline surface with a live metadata client pointed at
// metaURL (empty = disabled).
func newMetaEnv(t *testing.T, libraryRoots []string, metaURL string) *pipelineEnv {
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
	cfg.Metadata.BaseURL = metaURL

	hub := events.NewHub(64)
	sched := scheduler.New(db, hub, scheduler.NewStubExecutor(0, 0), 2, t.TempDir(), false)
	meta := metaops.NewClient(metaURL)
	scans := metaops.NewScanManager(context.Background(), meta, "", storeOverrides(db))

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
		Meta:      meta,
		Config:    cfg,
	})
	env.srv = httptest.NewServer(env.api.Handler())
	t.Cleanup(env.srv.Close)
	return &pipelineEnv{testEnv: env, db: db, cfg: cfg}
}

func TestOverridesHideListAndClear(t *testing.T) {
	env := newMetaEnv(t, nil, "")
	token := env.login(t)

	// Hide a book.
	resp := env.do(t, http.MethodPost, "/api/v1/overrides", token,
		`{"source_path":"/lib/a","hidden":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hide = %d, want 200", resp.StatusCode)
	}
	var ur upsertOverrideResponse
	_ = json.NewDecoder(resp.Body).Decode(&ur)
	resp.Body.Close()
	if !ur.Override.Hidden || ur.Coverage != nil {
		t.Fatalf("hide response = %+v", ur)
	}

	// List shows it.
	resp = env.do(t, http.MethodGet, "/api/v1/overrides", token, "")
	var lr listOverridesResponse
	_ = json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if len(lr.Overrides) != 1 || lr.Overrides[0].SourcePath != "/lib/a" || !lr.Overrides[0].Hidden {
		t.Fatalf("list = %+v", lr.Overrides)
	}

	// Clear it (hidden=false, no work_id) -> deletes the row.
	resp = env.do(t, http.MethodPost, "/api/v1/overrides", token,
		`{"source_path":"/lib/a","hidden":false}`)
	resp.Body.Close()
	resp = env.do(t, http.MethodGet, "/api/v1/overrides", token, "")
	_ = json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if len(lr.Overrides) != 0 {
		t.Fatalf("after clear = %+v, want empty", lr.Overrides)
	}
}

func TestOverrideManualMatchAndResolveFailure(t *testing.T) {
	meta := fakeMeta(t)
	env := newMetaEnv(t, nil, meta.URL)
	token := env.login(t)

	// A resolvable work id -> 200 with manual coverage + persisted work_title.
	resp := env.do(t, http.MethodPost, "/api/v1/overrides", token,
		`{"source_path":"/lib/m","hidden":false,"work_id":"w-good"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manual match = %d, want 200", resp.StatusCode)
	}
	var ur upsertOverrideResponse
	_ = json.NewDecoder(resp.Body).Decode(&ur)
	resp.Body.Close()
	if ur.Coverage == nil || ur.Coverage.MatchedBy != "manual" || ur.Coverage.WorkID != "w-good" {
		t.Fatalf("manual coverage = %+v", ur.Coverage)
	}
	if ur.Override.WorkID != "w-good" || ur.Override.WorkTitle != "Good Work" {
		t.Fatalf("persisted override = %+v", ur.Override)
	}

	// A stale/unknown work id -> 400 (clean upstream 404).
	resp = env.do(t, http.MethodPost, "/api/v1/overrides", token,
		`{"source_path":"/lib/m","work_id":"w-bad"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad work id = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// work_id set but metadata disabled -> 503.
	off := newMetaEnv(t, nil, "")
	offToken := off.login(t)
	resp = off.do(t, http.MethodPost, "/api/v1/overrides", offToken,
		`{"source_path":"/lib/m","work_id":"w-good"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("work_id with metadata off = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestOverridePathAllowList is the security regression: an override is refused
// (403) outside library_roots and accepted inside (both an allowed and a denied
// case, per the security-critical-path convention).
func TestOverridePathAllowList(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "book-a")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "book-b")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	env := newMetaEnv(t, []string{root}, "")
	token := env.login(t)

	// Allowed: inside a configured root.
	resp := env.do(t, http.MethodPost, "/api/v1/overrides", token,
		`{"source_path":"`+inside+`","hidden":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("inside-root override = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Denied: outside every root -> 403, and nothing is persisted.
	resp = env.do(t, http.MethodPost, "/api/v1/overrides", token,
		`{"source_path":"`+outside+`","hidden":true}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("outside-root override = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Empty source_path -> 400.
	resp = env.do(t, http.MethodPost, "/api/v1/overrides", token, `{"source_path":"","hidden":true}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty source_path = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	if list, _ := env.db.ListOverrides(context.Background()); len(list) != 1 {
		t.Errorf("persisted overrides = %d, want only the allowed one", len(list))
	}
}

func TestMetaSearchProxy(t *testing.T) {
	meta := fakeMeta(t)
	env := newMetaEnv(t, nil, meta.URL)
	token := env.login(t)

	// Work hits only, authors flattened to names.
	resp := env.do(t, http.MethodGet, "/api/v1/meta/search?q=good", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search = %d, want 200", resp.StatusCode)
	}
	var sr metaSearchResponse
	_ = json.NewDecoder(resp.Body).Decode(&sr)
	resp.Body.Close()
	if len(sr.Results) != 1 || sr.Results[0].ID != "w-good" || len(sr.Results[0].Authors) != 1 {
		t.Fatalf("search results = %+v", sr.Results)
	}

	// Empty q -> 400.
	resp = env.do(t, http.MethodGet, "/api/v1/meta/search?q=", token, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty q = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Metadata disabled -> 503.
	off := newMetaEnv(t, nil, "")
	offToken := off.login(t)
	resp = off.do(t, http.MethodGet, "/api/v1/meta/search?q=x", offToken, "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("search with metadata off = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()

	// Upstream down -> 502.
	bad := newMetaEnv(t, nil, "http://127.0.0.1:0")
	badToken := bad.login(t)
	resp = bad.do(t, http.MethodGet, "/api/v1/meta/search?q=x", badToken, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("search with upstream down = %d, want 502", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestMetaSearchLimitBoundary proves the boundary limit value (== metaops.SearchLimit)
// is honored and forwarded, not clamped away by an exclusive bound.
func TestMetaSearchLimitBoundary(t *testing.T) {
	var gotLimit string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`{"results":[` +
			`{"kind":"work","id":"w-good","title":"Good Work","authors":[],"series":null,"cover_url":""}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	env := newMetaEnv(t, nil, srv.URL)
	token := env.login(t)

	resp := env.do(t, http.MethodGet, "/api/v1/meta/search?q=good&limit=20", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("limit=20 = %d, want 200", resp.StatusCode)
	}
	var sr metaSearchResponse
	_ = json.NewDecoder(resp.Body).Decode(&sr)
	resp.Body.Close()
	if len(sr.Results) != 1 {
		t.Fatalf("results = %+v", sr.Results)
	}
	if gotLimit != "20" {
		t.Fatalf("forwarded limit = %q, want 20 (boundary honored)", gotLimit)
	}
}

func TestListScansEndpoint(t *testing.T) {
	root := t.TempDir()
	env := newMetaEnv(t, []string{root}, "")
	token := env.login(t)

	// No scans yet -> empty list.
	resp := env.do(t, http.MethodGet, "/api/v1/scans", token, "")
	var lr listScansResponse
	_ = json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if len(lr.Scans) != 0 {
		t.Fatalf("initial scans = %+v", lr.Scans)
	}

	// Start a scan, then the list reattaches to it (no book list in the summary).
	resp = env.do(t, http.MethodPost, "/api/v1/scans", token, `{"path":"`+root+`"}`)
	var cr createScanResponse
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()

	resp = env.do(t, http.MethodGet, "/api/v1/scans", token, "")
	_ = json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if len(lr.Scans) != 1 || lr.Scans[0].ID != cr.JobID {
		t.Fatalf("scans list = %+v (want the started job %q)", lr.Scans, cr.JobID)
	}
	if lr.Scans[0].StartedAt == "" {
		t.Error("scan summary missing started_at")
	}
}

func TestBookWorkIDPersistsThroughCreate(t *testing.T) {
	env := newMetaEnv(t, nil, "")
	token := env.login(t)
	resp := env.do(t, http.MethodPost, "/api/v1/books", token,
		`{"candidates":[{"source_path":"/b/w","title":"W","work_id":"work-9"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create = %d, want 200", resp.StatusCode)
	}
	var cr createBooksResponse
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	if len(cr.Results) != 1 || cr.Results[0].Book == nil || cr.Results[0].Book.WorkID != "work-9" {
		t.Fatalf("work_id not persisted through create: %+v", cr.Results)
	}
}
