package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/canonical"
	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/contrib"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// --- fakes -------------------------------------------------------------------

// fakeTokenResolver is a deterministic TokenResolver (the real gh-auth fallback would
// make a no-credential test flaky on a host with gh installed).
type fakeTokenResolver struct {
	token string
	err   error
}

func (f fakeTokenResolver) Resolve(context.Context) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.token, "pat", nil
}

// createdIssue records an issue the fake GitHub server was asked to open.
type createdIssue struct {
	number int
	title  string
	body   string
	labels []string
}

// fakeGitHub stands in for api.github.com for the contribution client.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	issues      []createdIssue
	gists       int
	forks       int
	puts        []string        // contents paths PUT (also the "file exists" set for GET contents)
	pulls       int             // POST /pulls count (a resume must not add another)
	refs        int             // POST /git/refs count (a resume must not re-create the branch)
	branches    map[string]bool // branches created on the fork
	openPRHeads map[string]int  // open PR head -> number
	dropLabels  bool            // GET issue omits the routing labels (non-collaborator drop)
	rateLimit   bool            // creation calls return a 403 rate-limit
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{t: t, branches: map[string]bool{}, openPRHeads: map[string]int{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := r.URL.Path

	// Rate-limit creation calls (never GETs) when configured.
	if g.rateLimit && r.Method == http.MethodPost {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		return
	}

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/issues"):
		var req struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		g.decode(r, &req)
		num := len(g.issues) + 1
		g.issues = append(g.issues, createdIssue{number: num, title: req.Title, body: req.Body, labels: req.Labels})
		g.writeIssue(w, http.StatusCreated, num, req.Labels)

	case r.Method == http.MethodGet && strings.Contains(path, "/issues/"):
		num := lastInt(path)
		var labels []string
		if num >= 1 && num <= len(g.issues) {
			labels = g.issues[num-1].labels
		}
		if g.dropLabels {
			labels = keepOnly(labels, "data") // routing labels silently dropped
		}
		g.writeIssue(w, http.StatusOK, num, labels)

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/gists"):
		g.gists++
		g.writeJSON(w, http.StatusCreated, map[string]any{
			"files": map[string]any{
				"characters.json": map[string]string{"raw_url": g.srv.URL + "/gist/characters.json"},
				"recaps.json":     map[string]string{"raw_url": g.srv.URL + "/gist/recaps.json"},
			},
		})

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/forks"):
		g.forks++
		g.writeJSON(w, http.StatusAccepted, map[string]string{"full_name": "tester/audiosilo-meta"})
	case r.Method == http.MethodGet && path == "/repos/tester/audiosilo-meta":
		g.writeJSON(w, http.StatusOK, map[string]string{"full_name": "tester/audiosilo-meta"})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/merge-upstream"):
		g.writeJSON(w, http.StatusOK, map[string]string{})
	case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/"):
		branch := strings.SplitN(path, "/git/ref/heads/", 2)[1]
		if branch == "main" || g.branches[branch] {
			g.writeJSON(w, http.StatusOK, map[string]any{"object": map[string]string{"sha": "deadbeef"}})
			return
		}
		w.WriteHeader(http.StatusNotFound) // BranchRef: branch not created yet
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/refs"):
		var req struct {
			Ref string `json:"ref"`
		}
		g.decode(r, &req)
		g.refs++
		g.branches[strings.TrimPrefix(req.Ref, "refs/heads/")] = true
		g.writeJSON(w, http.StatusCreated, map[string]string{"ref": req.Ref})
	case r.Method == http.MethodGet && strings.Contains(path, "/contents/"):
		file := strings.SplitN(path, "/contents/", 2)[1]
		if contains(g.puts, file) {
			g.writeJSON(w, http.StatusOK, map[string]string{"sha": "blob-sha"}) // file exists -> update
			return
		}
		w.WriteHeader(http.StatusNotFound) // contentSHA: file not present -> create
	case r.Method == http.MethodPut && strings.Contains(path, "/contents/"):
		g.puts = append(g.puts, strings.SplitN(path, "/contents/", 2)[1])
		g.writeJSON(w, http.StatusCreated, map[string]any{"content": map[string]string{"path": "x"}})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls"):
		if num, ok := g.openPRHeads[r.URL.Query().Get("head")]; ok {
			g.writeJSON(w, http.StatusOK, []map[string]any{
				{"number": num, "html_url": g.srv.URL + fmt.Sprintf("/pull/%d", num), "state": "open"},
			})
			return
		}
		g.writeJSON(w, http.StatusOK, []any{}) // FindOpenPRByHead: none yet
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/pulls"):
		g.pulls++
		var req struct {
			Head string `json:"head"`
		}
		g.decode(r, &req)
		g.openPRHeads[req.Head] = 42
		g.writeJSON(w, http.StatusCreated, map[string]any{"number": 42, "html_url": g.srv.URL + "/pull/42"})

	default:
		g.t.Errorf("fakeGitHub: unhandled %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (g *fakeGitHub) writeIssue(w http.ResponseWriter, status, num int, labels []string) {
	ls := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		ls = append(ls, map[string]string{"name": l})
	}
	g.writeJSON(w, status, map[string]any{
		"number":   num,
		"html_url": g.srv.URL + fmt.Sprintf("/issue/%d", num),
		"labels":   ls,
	})
}

func (g *fakeGitHub) decode(r *http.Request, v any) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		g.t.Fatalf("fakeGitHub: decode body: %v", err)
	}
}

func (g *fakeGitHub) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (g *fakeGitHub) issueCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.issues)
}

// fakeMeta stands in for meta.audiosilo.app (the metaops client's upstream).
type fakeMeta struct {
	srv *httptest.Server
	// lookups maps "asin:<v>"/"isbn:<v>" -> work id.
	lookups map[string]string
	// works maps work id -> (title, hasChars, hasRecaps); absent id => 404.
	works map[string]metaWorkFixture
}

type metaWorkFixture struct {
	title     string
	hasChars  bool
	hasRecaps bool
}

func newFakeMeta(t *testing.T, lookups map[string]string, works map[string]metaWorkFixture) *fakeMeta {
	m := &fakeMeta{lookups: lookups, works: works}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMeta) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/lookup":
		id := ""
		if a := r.URL.Query().Get("asin"); a != "" {
			id = m.lookups["asin:"+a]
		}
		if id == "" {
			if b := r.URL.Query().Get("isbn"); b != "" {
				id = m.lookups["isbn:"+b]
			}
		}
		if id == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"work": nil})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"work": map[string]string{"id": id}})

	case strings.HasPrefix(r.URL.Path, "/api/v1/works/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/works/")
		wf, ok := m.works[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		out := map[string]any{"title": wf.title}
		if wf.hasChars {
			out["characters"] = []map[string]string{{"id": "x"}}
		}
		if wf.hasRecaps {
			out["recaps"] = []map[string]any{{"through": map[string]int{"chapter": 1}}}
		}
		_ = json.NewEncoder(w).Encode(out)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// --- helpers -----------------------------------------------------------------

const testRepo = "KodeStar/audiosilo-meta"

func openContribDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// contribBook creates a book row whose work dir is populated with a valid sidecar pair.
func contribBook(t *testing.T, db *store.DB, nb store.NewBook, chars *model.Characters, recs *model.Recaps) store.Book {
	t.Helper()
	work := t.TempDir()
	nb.WorkDir = work
	if nb.SourcePath == "" {
		nb.SourcePath = "/src/" + work
	}
	if nb.Title == "" {
		nb.Title = "The Test Book"
	}
	seedWorkSidecars(t, work, chars, recs)
	b, err := db.CreateBook(context.Background(), nb)
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	return b
}

func contribConfig(t *testing.T, db *store.DB, mode, ghURL, metaURL, exportRoot string, tok TokenResolver) Config {
	t.Helper()
	var meta MetaCoverage
	if metaURL != "" {
		meta = metaops.NewClient(metaURL)
	}
	return Config{
		DB:             db,
		DataDir:        t.TempDir(),
		Fallback:       scheduler.NewStubExecutor(0, 0),
		Meta:           meta,
		TokenSource:    tok,
		ContribMode:    mode,
		ContribRepo:    testRepo,
		ContribBaseURL: ghURL,
		ExportRoot:     exportRoot,
	}
}

func rowsByKind(t *testing.T, db *store.DB, bookID int64) map[string]store.Contribution {
	t.Helper()
	rows, err := db.ListContributionsByBook(context.Background(), bookID)
	if err != nil {
		t.Fatalf("list contributions: %v", err)
	}
	m := map[string]store.Contribution{}
	for _, r := range rows {
		m[r.Kind] = r
	}
	return m
}

func lastInt(path string) int {
	seg := path[strings.LastIndexByte(path, '/')+1:]
	n := 0
	for _, c := range seg {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func keepOnly(labels []string, keep ...string) []string {
	set := map[string]bool{}
	for _, k := range keep {
		set[k] = true
	}
	var out []string
	for _, l := range labels {
		if set[l] {
			out = append(out, l)
		}
	}
	return out
}

// --- tests -------------------------------------------------------------------

func TestContributeIssueHappyPath(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{
		"reacher-01": {title: "Killing Floor"}, // no sidecars yet
	})

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("placeholder"), baseRecaps("placeholder"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	exe := NewExecutor(cfg)

	res, err := exe.Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if res.RateSample == nil {
		t.Error("expected a whole-book RateSample")
	}
	if !scheduler.SentinelExists(b.WorkDir, string(state.Contributing)) {
		t.Error("contributing sentinel not written")
	}
	if gh.issueCount() != 2 {
		t.Fatalf("issues created = %d, want 2", gh.issueCount())
	}

	// The on-disk sidecars were reconciled to the real slug + stamped source, canonical.
	for _, name := range []string{charactersFileName, recapsFileName} {
		raw, err := os.ReadFile(filepath.Join(b.WorkDir, sidecarsDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"work": "reacher-01"`) {
			t.Errorf("%s: work not rewritten to slug:\n%s", name, raw)
		}
		if !strings.Contains(string(raw), contribSourceRef) {
			t.Errorf("%s: contribution source not stamped", name)
		}
		if f, _ := canonical.Format(raw); !bytes.Equal(raw, f) {
			t.Errorf("%s: not canonical on disk", name)
		}
	}

	// Issue bodies carry the fenced payload with the rewritten slug + the routing labels.
	gotChars, gotRecaps := false, false
	for _, iss := range gh.issues {
		if !strings.Contains(iss.body, "```json") {
			t.Errorf("issue %q missing fenced payload", iss.title)
		}
		if !strings.Contains(iss.body, `"work": "reacher-01"`) {
			t.Errorf("issue %q missing rewritten work slug", iss.title)
		}
		switch {
		case contains(iss.labels, "data:characters"):
			gotChars = true
		case contains(iss.labels, "data:recaps"):
			gotRecaps = true
		}
	}
	if !gotChars || !gotRecaps {
		t.Errorf("expected both characters+recaps labelled issues (chars=%v recaps=%v)", gotChars, gotRecaps)
	}

	// Both rows recorded submitted, no note.
	rows := rowsByKind(t, db, b.ID)
	for _, k := range []string{store.ContribKindCharacters, store.ContribKindRecaps} {
		r := rows[k]
		if r.Status != store.ContribStatusSubmitted || r.URL == "" || r.Note != "" {
			t.Errorf("%s row = %+v, want submitted with url and no note", k, r)
		}
	}
}

func TestContributeIssueLabelsDropped(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	gh.dropLabels = true

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	rows := rowsByKind(t, db, b.ID)
	if note := rows[store.ContribKindCharacters].Note; !strings.Contains(note, "labels missing") {
		t.Errorf("characters note = %q, want a labels-missing note", note)
	}
}

func TestContributeIssueOversizePayloadUsesGist(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)

	huge := baseChars("x")
	huge.Characters[0].Description = strings.Repeat("word ", 15000) // ~75 KB > 60 KB body limit
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, huge, baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if gh.gists != 1 {
		t.Fatalf("gists created = %d, want 1", gh.gists)
	}
	// The characters issue links the gist raw URL instead of inlining the payload.
	var charsBody string
	for _, iss := range gh.issues {
		if contains(iss.labels, "data:characters") {
			charsBody = iss.body
		}
	}
	if !strings.Contains(charsBody, gh.srv.URL+"/gist/characters.json") {
		t.Errorf("characters issue did not link the gist:\n%s", truncate(charsBody))
	}
	if strings.Contains(charsBody, "```json") {
		t.Error("characters issue inlined an oversize payload instead of linking a gist")
	}
}

func TestContributeSkipsCoveredDimension(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{
		"reacher-01": {title: "Killing Floor", hasChars: true}, // characters already upstream
	})

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if gh.issueCount() != 1 {
		t.Fatalf("issues created = %d, want 1 (recaps only)", gh.issueCount())
	}
	rows := rowsByKind(t, db, b.ID)
	if rows[store.ContribKindCharacters].Status != store.ContribStatusAlreadyCovered {
		t.Errorf("characters row = %+v, want already_covered", rows[store.ContribKindCharacters])
	}
	if rows[store.ContribKindRecaps].Status != store.ContribStatusSubmitted {
		t.Errorf("recaps row = %+v, want submitted", rows[store.ContribKindRecaps])
	}
}

func TestContributePRMode(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModePR, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if gh.forks != 1 || gh.pulls != 1 || len(gh.puts) != 2 {
		t.Fatalf("pr flow: forks=%d pulls=%d puts=%d, want 1/1/2", gh.forks, gh.pulls, len(gh.puts))
	}
	// Contents committed at the canonical data/works/<shard>/<slug>/ paths.
	shard := model.Shard("reacher-01")
	wantChars := fmt.Sprintf("data/works/%s/reacher-01/characters.json", shard)
	wantRecaps := fmt.Sprintf("data/works/%s/reacher-01/recaps.json", shard)
	if !contains(gh.puts, wantChars) || !contains(gh.puts, wantRecaps) {
		t.Errorf("PutContents paths = %v, want %s + %s", gh.puts, wantChars, wantRecaps)
	}
	// Both rows share the one PR url/number.
	rows := rowsByKind(t, db, b.ID)
	c, r := rows[store.ContribKindCharacters], rows[store.ContribKindRecaps]
	if c.URL == "" || c.URL != r.URL || c.Number != r.Number || c.Status != store.ContribStatusSubmitted {
		t.Errorf("pr rows do not share the PR: chars=%+v recaps=%+v", c, r)
	}
}

func TestContributeLocalMode(t *testing.T) {
	db := openContribDB(t)
	export := t.TempDir()

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeLocal, "", "", export, nil)
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	shard := model.Shard("reacher-01")
	for _, name := range []string{charactersFileName, recapsFileName} {
		p := filepath.Join(export, "works", shard, "reacher-01", name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("local export missing %s: %v", p, err)
		}
	}
	rows := rowsByKind(t, db, b.ID)
	if rows[store.ContribKindCharacters].Status != store.ContribStatusLocal {
		t.Errorf("characters row = %+v, want local", rows[store.ContribKindCharacters])
	}
}

// TestContributeCarriesAuditAcceptanceNote: a book whose sidecars were accepted on a
// converging audit trajectory (audit_accepted.json present) carries the residual-nits
// note on its contribution rows, so the acceptance surfaces in the UI.
func TestContributeCarriesAuditAcceptanceNote(t *testing.T) {
	db := openContribDB(t)
	export := t.TempDir()

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	if err := writeAuditAccepted(b.WorkDir, auditAccepted{Round: 2, Fix: 1, Nit: 3}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeLocal, "", "", export, nil)
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	rows := rowsByKind(t, db, b.ID)
	for kind, row := range rows {
		if !strings.Contains(row.Note, "converged after 2 rounds") || !strings.Contains(row.Note, "3 residual nit") {
			t.Errorf("%s row note = %q, want the acceptance line", kind, row.Note)
		}
	}
}

func TestContributeLocalModeUnresolvedSlugUsesPlaceholder(t *testing.T) {
	db := openContribDB(t)
	export := t.TempDir()

	// No WorkID and no meta => local mode falls back to a title-derived placeholder slug.
	b := contribBook(t, db, store.NewBook{Title: "Some Unknown Book"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeLocal, "", "", export, nil)
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	slug := "some-unknown-book"
	p := filepath.Join(export, "works", model.Shard(slug), slug, charactersFileName)
	if _, err := os.Stat(p); err != nil {
		t.Errorf("placeholder export missing %s: %v", p, err)
	}
	rows := rowsByKind(t, db, b.ID)
	if note := rows[store.ContribKindCharacters].Note; note == "" {
		t.Error("expected a placeholder note on the local row")
	}
}

func TestContributeStaleWorkIDFallsToAsinLookup(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	// The recorded WorkID is stale (404 upstream); the ASIN resolves to the real work.
	meta := newFakeMeta(t,
		map[string]string{"asin:B01": "reacher-01"},
		map[string]metaWorkFixture{"reacher-01": {title: "Killing Floor"}},
	)

	b := contribBook(t, db, store.NewBook{WorkID: "stale-slug", ASIN: "B01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	// The resolved work id is persisted back onto the book.
	got, _ := db.GetBook(context.Background(), b.ID)
	if got.WorkID != "reacher-01" {
		t.Errorf("work_id = %q, want reacher-01 (asin match persisted)", got.WorkID)
	}
	if gh.issueCount() != 2 {
		t.Errorf("issues = %d, want 2", gh.issueCount())
	}
}

func TestContributeNoMatchParksCoreNeeded(t *testing.T) {
	db := openContribDB(t)
	meta := newFakeMeta(t, nil, nil) // nothing resolves

	b := contribBook(t, db, store.NewBook{Title: "Mystery Book", ASIN: "BZZ"}, baseChars("x"), baseRecaps("x"))
	// Seed the language + runtime the core proposal prefills from.
	if err := writeASRProvenance(b.WorkDir, asrProvenance{Language: "en"}); err != nil {
		t.Fatal(err)
	}
	if err := audio.WriteManifest(b.WorkDir, audio.Manifest{Style: audio.StyleMarkers, Duration: 3600, ChapterCount: 1}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeIssue, "", meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})

	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	assertPark(t, err, state.ParkCoreNeeded)

	// A prefilled proposal is written with the language + runtime.
	raw, rerr := os.ReadFile(filepath.Join(b.WorkDir, contribDir, coreProposalName))
	if rerr != nil {
		t.Fatalf("core proposal not written: %v", rerr)
	}
	var p contrib.CoreProposal
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.Title != "Mystery Book" || p.Language != "en" || p.RuntimeMin != 60 {
		t.Errorf("core proposal prefill = %+v, want title/language=en/runtime=60", p)
	}
	if len(p.ASINs) != 1 || p.ASINs[0].ASIN != "BZZ" || p.ASINs[0].Region != "" {
		t.Errorf("core proposal ASINs = %+v, want one BZZ with empty region", p.ASINs)
	}
}

func TestContributeCorePendingWhenProposalSubmitted(t *testing.T) {
	db := openContribDB(t)
	b := contribBook(t, db, store.NewBook{Title: "Mystery Book"}, baseChars("x"), baseRecaps("x"))
	// A core proposal was already submitted (the confirm endpoint's row).
	if _, err := db.UpsertContribution(context.Background(), store.Contribution{
		BookID: b.ID, Kind: store.ContribKindCore, Mode: store.ContribModeIssue,
		Status: store.ContribStatusSubmitted, Number: 7, URL: "https://x/7",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeIssue, "", "", "", fakeTokenResolver{token: "ghp_x"})
	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	assertPark(t, err, state.ParkCorePending)
}

func TestContributeNoCredentialParks(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))

	// Denied: no credential parks contrib_unavailable, no issue created.
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{err: contrib.ErrNoCredential})
	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	assertPark(t, err, state.ParkContribUnavailable)
	if gh.issueCount() != 0 {
		t.Errorf("issues created = %d, want 0 when uncredentialed", gh.issueCount())
	}

	// Allowed: a PAT proceeds.
	b2 := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg2 := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_ok"})
	if _, err := NewExecutor(cfg2).Execute(context.Background(), b2, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute with PAT: %v", err)
	}
	if gh.issueCount() != 2 {
		t.Errorf("issues created = %d, want 2 with a PAT", gh.issueCount())
	}
}

func TestContributeResumeSkipsAlreadyPostedRows(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	// Both dimensions already posted (a crash between submit and sentinel).
	for _, k := range []string{store.ContribKindCharacters, store.ContribKindRecaps} {
		if _, err := db.UpsertContribution(context.Background(), store.Contribution{
			BookID: b.ID, Kind: k, Mode: store.ContribModeIssue,
			Status: store.ContribStatusSubmitted, Number: 1, URL: "https://x/1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if gh.issueCount() != 0 {
		t.Errorf("resume created %d issues, want 0", gh.issueCount())
	}
}

func TestContributeRateLimitIsTransientNotPark(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	gh.rateLimit = true
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})

	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	var rl *contrib.RateLimitError
	if !errors.As(err, &rl) {
		t.Errorf("err = %v, want a *contrib.RateLimitError", err)
	}
	var pe *scheduler.ParkError
	if errors.As(err, &pe) {
		t.Errorf("rate limit must NOT be a park, got %v", pe)
	}
}

// TestContributePRResumeReusesBranchAndPR simulates a first run that created the
// branch + files + PR but persisted no rows (a crash before the rows/sentinel), then
// re-runs and asserts the branch and PR are REUSED (not duplicated) and the rows are
// persisted with the existing PR's URL. This guards the M7 crash-resume idempotency.
func TestContributePRResumeReusesBranchAndPR(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))

	// First run with NO db -> it creates the branch/files/PR on the fake but persists no
	// contribution rows (the crash window).
	cfg1 := contribConfig(t, db, contribModePR, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})
	cfg1.DB = nil
	if _, err := NewExecutor(cfg1).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if gh.forks == 0 || gh.pulls != 1 || gh.refs != 1 {
		t.Fatalf("first run: forks=%d pulls=%d refs=%d, want forks>=1 pulls=1 refs=1", gh.forks, gh.pulls, gh.refs)
	}
	if n := len(rowsByKind(t, db, b.ID)); n != 0 {
		t.Fatalf("first run persisted %d rows, want 0 (no db)", n)
	}

	// Second run WITH the db: it must reuse the existing branch (no new ref) and the
	// existing open PR (no new pull), and persist both rows pointing at that PR.
	cfg2 := contribConfig(t, db, contribModePR, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg2).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if gh.refs != 1 {
		t.Errorf("branch re-created on resume: refs=%d, want 1", gh.refs)
	}
	if gh.pulls != 1 {
		t.Errorf("duplicate PR opened on resume: pulls=%d, want 1", gh.pulls)
	}
	rows := rowsByKind(t, db, b.ID)
	wantURL := gh.srv.URL + "/pull/42"
	for _, k := range []string{store.ContribKindCharacters, store.ContribKindRecaps} {
		if rows[k].URL != wantURL || rows[k].Status != store.ContribStatusSubmitted {
			t.Errorf("%s row = %+v, want submitted with url %s", k, rows[k], wantURL)
		}
	}
}

// TestContributeSettledRowNotOverwrittenByCovered proves a settled (submitted) row is
// never clobbered by a later upstream-covered verdict: the rowSettled check runs BEFORE
// the covered branch.
func TestContributeSettledRowNotOverwrittenByCovered(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{
		"reacher-01": {title: "Killing Floor", hasChars: true}, // characters now covered upstream
	})
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	// A prior run already submitted the characters dimension.
	if _, err := db.UpsertContribution(context.Background(), store.Contribution{
		BookID: b.ID, Kind: store.ContribKindCharacters, Mode: store.ContribModeIssue,
		Status: store.ContribStatusSubmitted, Number: 5, URL: "https://x/5",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	rows := rowsByKind(t, db, b.ID)
	if r := rows[store.ContribKindCharacters]; r.Status != store.ContribStatusSubmitted || r.URL != "https://x/5" {
		t.Errorf("settled characters row was overwritten by the covered verdict: %+v", r)
	}
	if rows[store.ContribKindRecaps].Status != store.ContribStatusSubmitted {
		t.Errorf("recaps row = %+v, want submitted", rows[store.ContribKindRecaps])
	}
}

// TestContributeMergedCoreTrustsWorkIDOn404 proves a book whose work id 404s upstream
// but has a MERGED core row proceeds on that trusted slug (the merged intake PR created
// it; the data release just has not rebuilt yet) instead of re-parking needs-core.
func TestContributeMergedCoreTrustsWorkIDOn404(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, nil) // every work id 404s
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	if _, err := db.UpsertContribution(context.Background(), store.Contribution{
		BookID: b.ID, Kind: store.ContribKindCore, Mode: store.ContribModeIssue,
		Status: store.ContribStatusMerged, Number: 9, URL: "https://x/9",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute should proceed on the merged-core slug, got: %v", err)
	}
	if gh.issueCount() != 2 {
		t.Errorf("issues = %d, want 2 (both dims submitted with the trusted slug)", gh.issueCount())
	}
	raw, _ := os.ReadFile(filepath.Join(b.WorkDir, sidecarsDir, charactersFileName))
	if !strings.Contains(string(raw), `"work": "reacher-01"`) {
		t.Errorf("characters not reconciled to the trusted slug:\n%s", raw)
	}
}

// --- small assertion helpers -------------------------------------------------

func assertPark(t *testing.T, err error, want state.ParkCode) {
	t.Helper()
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a ParkError with code %s", err, want)
	}
	if pe.Code != want {
		t.Fatalf("park code = %s, want %s", pe.Code, want)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
