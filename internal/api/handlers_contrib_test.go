package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/contrib"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// apiTokenResolver is a deterministic contrib.TokenResolver for the api tests.
type apiTokenResolver struct {
	token string
	err   error
}

func (r apiTokenResolver) Resolve(context.Context) (string, string, error) {
	if r.err != nil {
		return "", "", r.err
	}
	return r.token, contrib.FromPAT, nil
}

// withContrib returns an option wiring a Contrib service over a fake GitHub base URL.
// readmit defaults to the scheduler's Retry when nil.
func withContrib(ghURL string, tok contrib.TokenResolver, verify func(context.Context, string) error, readmit func(context.Context, int64) error) func(*Deps) {
	return func(d *Deps) {
		rd := readmit
		if rd == nil {
			rd = d.Scheduler.Retry
		}
		d.Contrib = contrib.NewService(contrib.ServiceDeps{
			DB: d.Store, Repo: "KodeStar/audiosilo-meta", BaseURL: ghURL, Tokens: tok,
			Publish: func(u contrib.ContribUpdate) { _ = d.Events.PublishBook("contrib.update", u.BookID, u) },
			Readmit: rd, VerifyWork: verify, CorePendingMsg: "waiting for the metadata PR",
		})
	}
}

func createBook(t *testing.T, db *store.DB, title, workID, parkCode string) store.Book {
	t.Helper()
	b, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: "/lib/" + title, WorkDir: t.TempDir(), Title: title, WorkID: workID,
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	if parkCode != "" {
		if err := db.SetBookState(context.Background(), b.ID,
			string(state.Contributing), string(state.StatusNeedsAttention), "parked", parkCode); err != nil {
			t.Fatalf("park book: %v", err)
		}
		nb, err := db.GetBook(context.Background(), b.ID)
		if err != nil {
			t.Fatal(err)
		}
		return nb
	}
	return b
}

func writeWorkFile(t *testing.T, workDir, rel, content string) {
	t.Helper()
	path := filepath.Join(workDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- auth denied on every new endpoint ---

func TestContribEndpointsRequireAuth(t *testing.T) {
	env := newPipelineEnv(t, nil)
	b := createBook(t, env.db, "A", "the-work", "")
	id := strconv.FormatInt(b.ID, 10)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/books/" + id + "/contrib/core"},
		{http.MethodPost, "/api/v1/books/" + id + "/contribute/core"},
		{http.MethodPost, "/api/v1/books/" + id + "/work"},
		{http.MethodGet, "/api/v1/books/" + id + "/export"},
	}
	for _, c := range cases {
		resp := env.do(t, c.method, c.path, "", `{}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s no-auth = %d, want 401", c.method, c.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// --- contribute/core: wrong park 409, invalid 400, happy 200 + park flip ---

func TestContributeCore(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"number":50,"html_url":"https://gh/issues/50","labels":[{"name":"data"},{"name":"data:add-work"}]}`)
	}))
	defer gh.Close()

	env := newPipelineEnv(t, nil, withContrib(gh.URL, apiTokenResolver{token: "ghp_x"}, nil, nil))
	token := env.login(t)

	// Wrong park state (not parked core_needed) -> 409.
	notParked := createBook(t, env.db, "Running", "", "")
	resp := env.do(t, http.MethodPost, "/api/v1/books/"+strconv.FormatInt(notParked.ID, 10)+"/contribute/core", token,
		`{"title":"T","authors":["A"],"language":"en","narrators":["N"],"sources":"s"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("wrong-park = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	parked := createBook(t, env.db, "Needs Core", "", string(state.ParkCoreNeeded))
	pid := strconv.FormatInt(parked.ID, 10)

	// Invalid proposal (missing narrators) -> 400.
	resp = env.do(t, http.MethodPost, "/api/v1/books/"+pid+"/contribute/core", token,
		`{"title":"T","authors":["A"],"language":"en","sources":"s"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid proposal = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Happy path -> 200 with the created row; park flips to core_pending.
	resp = env.do(t, http.MethodPost, "/api/v1/books/"+pid+"/contribute/core", token,
		`{"title":"T","authors":["A"],"language":"en","narrators":["N"],"sources":"s"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("happy = %d, want 200", resp.StatusCode)
	}
	var row contributionRowView
	_ = json.NewDecoder(resp.Body).Decode(&row)
	resp.Body.Close()
	if row.Kind != store.ContribKindCore || row.Number != 50 || row.Status != store.ContribStatusSubmitted {
		t.Fatalf("row = %+v", row)
	}
	nb, _ := env.db.GetBook(context.Background(), parked.ID)
	if nb.ParkCode != string(state.ParkCorePending) {
		t.Fatalf("park_code = %q, want core_pending", nb.ParkCode)
	}
}

// TestContributeCoreNoCredential: no PAT -> 409 naming the remedies.
func TestContributeCoreNoCredential(t *testing.T) {
	env := newPipelineEnv(t, nil, withContrib("http://127.0.0.1:0", apiTokenResolver{err: contrib.ErrNoCredential}, nil, nil))
	token := env.login(t)
	b := createBook(t, env.db, "Needs Core", "", string(state.ParkCoreNeeded))
	resp := env.do(t, http.MethodPost, "/api/v1/books/"+strconv.FormatInt(b.ID, 10)+"/contribute/core", token,
		`{"title":"T","authors":["A"],"language":"en","narrators":["N"],"sources":"s"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("no-cred = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- work: bad slug 400, upstream 404 -> 400, happy sets work_id + readmits ---

func TestSetWorkEndpoint(t *testing.T) {
	var spy struct {
		sync.Mutex
		ids []int64
	}
	readmit := func(_ context.Context, id int64) error {
		spy.Lock()
		defer spy.Unlock()
		spy.ids = append(spy.ids, id)
		return nil
	}
	verify := func(_ context.Context, workID string) error {
		if workID == "ghost-work" {
			return contrib.ErrWorkNotFound
		}
		return nil
	}
	env := newPipelineEnv(t, nil, withContrib("", apiTokenResolver{err: contrib.ErrNoCredential}, verify, readmit))
	token := env.login(t)

	b := createBook(t, env.db, "Set Work", "", string(state.ParkCoreNeeded))
	bid := strconv.FormatInt(b.ID, 10)

	// Bad slug -> 400.
	resp := env.do(t, http.MethodPost, "/api/v1/books/"+bid+"/work", token, `{"work_id":"Not A Slug!"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad slug = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Upstream not found -> 400.
	resp = env.do(t, http.MethodPost, "/api/v1/books/"+bid+"/work", token, `{"work_id":"ghost-work"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("upstream-missing = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Happy -> 200, work_id set, book re-admitted.
	resp = env.do(t, http.MethodPost, "/api/v1/books/"+bid+"/work", token, `{"work_id":"real-work"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("happy = %d, want 200", resp.StatusCode)
	}
	var view bookView
	_ = json.NewDecoder(resp.Body).Decode(&view)
	resp.Body.Close()
	if view.WorkID != "real-work" {
		t.Fatalf("work_id = %q, want real-work", view.WorkID)
	}
	spy.Lock()
	got := append([]int64(nil), spy.ids...)
	spy.Unlock()
	if len(got) != 1 || got[0] != b.ID {
		t.Fatalf("readmit called with %v, want [%d]", got, b.ID)
	}
}

// --- contrib/core GET: 404 absent, 200 present ---

func TestGetCoreProposal(t *testing.T) {
	env := newPipelineEnv(t, nil, withContrib("", apiTokenResolver{err: contrib.ErrNoCredential}, nil, nil))
	token := env.login(t)

	b := createBook(t, env.db, "Proposal", "", string(state.ParkCoreNeeded))
	bid := strconv.FormatInt(b.ID, 10)

	// Absent -> 404.
	resp := env.do(t, http.MethodGet, "/api/v1/books/"+bid+"/contrib/core", token, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("absent = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Present -> 200 with the JSON.
	writeWorkFile(t, b.WorkDir, filepath.Join("contrib", "core_proposal.json"),
		`{"title":"Proposal","authors":["A"],"language":"en","narrators":[],"sources":"scan"}`)
	resp = env.do(t, http.MethodGet, "/api/v1/books/"+bid+"/contrib/core", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("present = %d, want 200", resp.StatusCode)
	}
	var p contrib.CoreProposal
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if p.Title != "Proposal" || p.Language != "en" {
		t.Fatalf("proposal = %+v", p)
	}
}

// --- export: 404 no sidecars, 200 zip with exactly the expected entries ---

func TestBookExport(t *testing.T) {
	env := newPipelineEnv(t, nil, withContrib("", apiTokenResolver{err: contrib.ErrNoCredential}, nil, nil))
	token := env.login(t)

	b := createBook(t, env.db, "Export Me", "my-work", "")
	bid := strconv.FormatInt(b.ID, 10)

	// No sidecars -> 404.
	resp := env.do(t, http.MethodGet, "/api/v1/books/"+bid+"/export", token, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no-sidecars = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// With both sidecars -> 200 zip with works/my/my-work/{characters,recaps}.json.
	writeWorkFile(t, b.WorkDir, filepath.Join("sidecars", "characters.json"), `{"work":"my-work","characters":[]}`)
	writeWorkFile(t, b.WorkDir, filepath.Join("sidecars", "recaps.json"), `{"work":"my-work","recaps":[]}`)
	resp = env.do(t, http.MethodGet, "/api/v1/books/"+bid+"/export", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="my-work-sidecars.zip"` {
		t.Fatalf("content-disposition = %q", cd)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	want := []string{"works/my/my-work/characters.json", "works/my/my-work/recaps.json"}
	if len(names) != 2 || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("zip entries = %v, want %v", names, want)
	}
}

// --- bookView contribution aggregate appears in list + detail ---

func TestBookViewContributionAggregate(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)

	b := createBook(t, env.db, "Contributed", "the-work", "")
	if _, err := env.db.UpsertContribution(context.Background(), store.Contribution{
		BookID: b.ID, Kind: store.ContribKindCharacters, Mode: store.ContribModeIssue,
		Repo: "r", Number: 7, URL: "https://gh/issues/7", Status: store.ContribStatusSubmitted,
	}); err != nil {
		t.Fatal(err)
	}

	// List: the aggregate chip is present.
	resp := env.do(t, http.MethodGet, "/api/v1/books", token, "")
	var list listBooksResponse
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	var found *bookView
	for i := range list.Books {
		if list.Books[i].ID == b.ID {
			found = &list.Books[i]
		}
	}
	if found == nil || found.Contribution == nil {
		t.Fatalf("list book contribution missing: %+v", found)
	}
	if found.Contribution.Status != store.ContribStatusSubmitted || found.Contribution.URL != "https://gh/issues/7" {
		t.Fatalf("list aggregate = %+v", found.Contribution)
	}

	// Detail: aggregate chip + full rows.
	resp = env.do(t, http.MethodGet, "/api/v1/books/"+strconv.FormatInt(b.ID, 10), token, "")
	var detail bookDetail
	_ = json.NewDecoder(resp.Body).Decode(&detail)
	resp.Body.Close()
	if detail.Contribution == nil || detail.Contribution.Status != store.ContribStatusSubmitted {
		t.Fatalf("detail aggregate = %+v", detail.Contribution)
	}
	if len(detail.Contributions) != 1 || detail.Contributions[0].Number != 7 {
		t.Fatalf("detail rows = %+v", detail.Contributions)
	}

	// An actionable row note also raises attention on the aggregate Done-board chip.
	rows, err := env.db.ListContributionsByBook(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("stored contribution rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if err := env.db.SetContributionStatus(context.Background(), row.ID, row.Status, row.PRNumber, row.PRURL, store.ContribNoteIntakePRStale); err != nil {
		t.Fatal(err)
	}
	resp = env.do(t, http.MethodGet, "/api/v1/books", token, "")
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	found = nil
	for i := range list.Books {
		if list.Books[i].ID == b.ID {
			found = &list.Books[i]
		}
	}
	if found == nil || found.Contribution == nil || !found.Contribution.Attention {
		t.Fatalf("stalled aggregate should need attention: %+v", found)
	}
}
