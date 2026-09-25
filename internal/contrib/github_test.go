package contrib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testRepo = "KodeStar/audiosilo-meta"

// newTestClient wires a Client to a handler-backed httptest server, closed at
// test end.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-token")
}

// readJSON decodes a request body into a map for assertions.
func readJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
}

func TestCreateIssue(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/"+testRepo+"/issues" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q", got)
		}
		body := readJSON(t, r)
		if body["title"] != "[characters] a-work" {
			t.Fatalf("title = %v", body["title"])
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"number":42,"html_url":"https://github.com/x/y/issues/42","labels":[{"name":"data"},{"name":"data:characters"}]}`)
	})
	iss, err := c.CreateIssue(context.Background(), testRepo, "[characters] a-work", "body", []string{"data", "data:characters"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if iss.Number != 42 || iss.URL != "https://github.com/x/y/issues/42" {
		t.Fatalf("issue = %+v", iss)
	}
	if len(iss.Labels) != 2 || iss.Labels[0] != "data" {
		t.Fatalf("labels = %v", iss.Labels)
	}
}

// TestLabelVerificationFlow: CreateIssue echoes the labels, but a later GetIssue
// shows them dropped (GitHub silently strips labels for a non-collaborator).
func TestLabelVerificationFlow(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/"+testRepo+"/issues":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":7,"html_url":"u","labels":[{"name":"data"},{"name":"data:characters"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+testRepo+"/issues/7":
			// Labels dropped.
			io.WriteString(w, `{"number":7,"html_url":"u","labels":[]}`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	created, err := c.CreateIssue(context.Background(), testRepo, "t", "b", []string{"data", "data:characters"})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Labels) != 2 {
		t.Fatalf("create should echo labels, got %v", created.Labels)
	}
	got, err := c.GetIssue(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Labels) != 0 {
		t.Fatalf("labels should read as dropped on GET, got %v", got.Labels)
	}
}

func TestCreateGist(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/gists" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body := readJSON(t, r)
		if body["public"] != false {
			t.Fatalf("secret gist must be public=false, got %v", body["public"])
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"files":{"characters.json":{"raw_url":"https://gist.githubusercontent.com/x/raw/y/characters.json"}}}`)
	})
	urls, err := c.CreateGist(context.Background(), map[string]string{"characters.json": "{}"}, true)
	if err != nil {
		t.Fatalf("CreateGist: %v", err)
	}
	if urls["characters.json"] != "https://gist.githubusercontent.com/x/raw/y/characters.json" {
		t.Fatalf("raw url = %v", urls)
	}
}

// TestEnsureFork exercises the POST-then-poll flow: the fork GET 404s once
// before becoming ready.
func TestEnsureFork(t *testing.T) {
	orig := forkPollInterval
	forkPollInterval = time.Millisecond // fast poll for the test
	t.Cleanup(func() { forkPollInterval = orig })
	fork := "tester/audiosilo-meta"
	var getCount int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/"+testRepo+"/forks":
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"full_name":"`+fork+`"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+fork:
			getCount++
			if getCount == 1 {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			io.WriteString(w, `{"full_name":"`+fork+`"}`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	got, err := c.EnsureFork(context.Background(), testRepo)
	if err != nil {
		t.Fatalf("EnsureFork: %v", err)
	}
	if got != fork {
		t.Fatalf("fork = %q", got)
	}
	if getCount < 2 {
		t.Fatalf("expected the readiness poll to retry, getCount=%d", getCount)
	}
}

func TestBranchSHAAndCreateRef(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+testRepo+"/git/ref/heads/main":
			io.WriteString(w, `{"object":{"sha":"deadbeef"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/"+testRepo+"/git/refs":
			body := readJSON(t, r)
			if body["ref"] != "refs/heads/sidecars/x-1" || body["sha"] != "deadbeef" {
				t.Fatalf("ref body = %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	sha, err := c.BranchSHA(context.Background(), testRepo, "main")
	if err != nil || sha != "deadbeef" {
		t.Fatalf("BranchSHA = %q, %v", sha, err)
	}
	if err := c.CreateRef(context.Background(), testRepo, "refs/heads/sidecars/x-1", sha); err != nil {
		t.Fatalf("CreateRef: %v", err)
	}
}

func TestMergeUpstream(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/tester/audiosilo-meta/merge-upstream" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body := readJSON(t, r)
		if body["branch"] != "main" {
			t.Fatalf("branch = %v", body["branch"])
		}
		io.WriteString(w, `{"merged":true}`)
	})
	if err := c.MergeUpstream(context.Background(), "tester/audiosilo-meta", "main"); err != nil {
		t.Fatalf("MergeUpstream: %v", err)
	}
}

func TestBranchRef(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/git/ref/heads/present"):
			io.WriteString(w, `{"object":{"sha":"cafe"}}`)
		case strings.HasSuffix(r.URL.Path, "/git/ref/heads/absent"):
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	sha, exists, err := c.BranchRef(context.Background(), testRepo, "present")
	if err != nil || !exists || sha != "cafe" {
		t.Fatalf("present branch = (%q, %v, %v)", sha, exists, err)
	}
	_, exists, err = c.BranchRef(context.Background(), testRepo, "absent")
	if err != nil || exists {
		t.Fatalf("absent branch = (exists=%v, err=%v), want (false, nil)", exists, err)
	}
}

// TestFindOpenPRByHead: an open PR for the head is returned found=true; an empty list is
// found=false.
func TestFindOpenPRByHead(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("head"); got != "tester:sidecars/x-1" {
			t.Fatalf("head = %q", got)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Fatalf("state = %q, want open", got)
		}
		io.WriteString(w, `[{"number":7,"html_url":"https://github.com/x/y/pull/7","state":"open"}]`)
	})
	pr, found, err := c.FindOpenPRByHead(context.Background(), testRepo, "tester:sidecars/x-1")
	if err != nil || !found || pr.Number != 7 {
		t.Fatalf("FindOpenPRByHead = (%+v, %v, %v)", pr, found, err)
	}

	c2 := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	if _, found, err := c2.FindOpenPRByHead(context.Background(), testRepo, "tester:none"); err != nil || found {
		t.Fatalf("empty = (found=%v, err=%v), want (false, nil)", found, err)
	}
}

func TestCreatePull(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/"+testRepo+"/pulls" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body := readJSON(t, r)
		if body["head"] != "tester:sidecars/x-1" || body["base"] != "main" {
			t.Fatalf("pull body = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"number":99,"html_url":"https://github.com/x/y/pull/99","state":"open"}`)
	})
	pr, err := c.CreatePull(context.Background(), testRepo, "tester:sidecars/x-1", "main", "title", "body")
	if err != nil {
		t.Fatalf("CreatePull: %v", err)
	}
	if pr.Number != 99 || pr.URL != "https://github.com/x/y/pull/99" {
		t.Fatalf("pr = %+v", pr)
	}
}

// TestFindIntakePR checks the head-branch query and merged_at -> Merged mapping.
func TestFindIntakePR(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/pulls" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("head"); got != "KodeStar:intake/issue-5" {
			t.Fatalf("head = %q", got)
		}
		if got := r.URL.Query().Get("state"); got != "all" {
			t.Fatalf("state = %q", got)
		}
		io.WriteString(w, `[{"number":123,"html_url":"https://github.com/x/y/pull/123","state":"closed","merged_at":"2026-07-17T00:00:00Z"}]`)
	})
	pr, found, err := c.FindIntakePR(context.Background(), testRepo, 5)
	if err != nil || !found {
		t.Fatalf("FindIntakePR found=%v err=%v", found, err)
	}
	if pr.Number != 123 || !pr.Merged {
		t.Fatalf("pr = %+v (merged_at should map to Merged)", pr)
	}
}

// TestFindIntakePRNone: an empty list is found=false, no error.
func TestFindIntakePRNone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	_, found, err := c.FindIntakePR(context.Background(), testRepo, 5)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected found=false for an empty list")
	}
}

func TestGetPullMerged(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/pulls/123" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, `{"number":123,"html_url":"u","state":"closed","merged":true,"merge_commit_sha":"m3rg3"}`)
	})
	pr, err := c.GetPull(context.Background(), testRepo, 123)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Merged || pr.State != "closed" || pr.MergeCommitSHA != "m3rg3" {
		t.Fatalf("pr = %+v", pr)
	}
}

// TestGetPullOpenHidesTestMergeSHA: an OPEN PR's merge_commit_sha is GitHub's
// test-merge commit, not a commit on the base branch, so it is not surfaced.
func TestGetPullOpenHidesTestMergeSHA(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"number":1,"html_url":"u","state":"open","merged":false,"merge_commit_sha":"test-merge"}`)
	})
	pr, err := c.GetPull(context.Background(), testRepo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pr.MergeCommitSHA != "" {
		t.Fatalf("open PR merge sha = %q, want empty", pr.MergeCommitSHA)
	}
}

func TestPullFiles(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/pulls/123/files" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") != "1" {
			t.Fatalf("page = %q, want 1 (a short page ends the listing)", r.URL.Query().Get("page"))
		}
		io.WriteString(w, `[{"filename":"data/works/c/cat.json","status":"modified"},`+
			`{"filename":"data/works/c/dog.json","status":"renamed","previous_filename":"data/works/c/cow.json"}]`)
	})
	files, err := c.PullFiles(context.Background(), testRepo, 123)
	if err != nil {
		t.Fatal(err)
	}
	want := []PullFile{
		{Filename: "data/works/c/cat.json", Status: "modified"},
		{Filename: "data/works/c/dog.json", Status: "renamed", PreviousFilename: "data/works/c/cow.json"},
	}
	if len(files) != 2 || files[0] != want[0] || files[1] != want[1] {
		t.Fatalf("files = %+v", files)
	}
}

// TestPullFilesPaginates: a full page of 100 asks for the next one.
func TestPullFilesPaginates(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			var b strings.Builder
			b.WriteString("[")
			for i := range 100 {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"filename":"f%d.json","status":"added"}`, i)
			}
			b.WriteString("]")
			io.WriteString(w, b.String())
		case "2":
			io.WriteString(w, `[{"filename":"last.json","status":"added"}]`)
		default:
			t.Fatalf("unexpected page %q", r.URL.Query().Get("page"))
		}
	})
	files, err := c.PullFiles(context.Background(), testRepo, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 101 || files[100].Filename != "last.json" {
		t.Fatalf("files = %d, last = %+v", len(files), files[len(files)-1])
	}
}

func TestFileAt(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
			t.Fatalf("Accept = %q, want the raw media type", r.Header.Get("Accept"))
		}
		if r.URL.Query().Get("ref") != "abc123" {
			t.Fatalf("ref = %q", r.URL.Query().Get("ref"))
		}
		switch r.URL.Path {
		case "/repos/" + testRepo + "/contents/data/works/0/0.json":
			io.WriteString(w, `{"entries":{}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	got, found, err := c.FileAt(context.Background(), testRepo, "data/works/0/0.json", "abc123")
	if err != nil || !found || string(got) != `{"entries":{}}` {
		t.Fatalf("FileAt = %q found=%v err=%v", got, found, err)
	}
	_, found, err = c.FileAt(context.Background(), testRepo, "data/works/0/gone.json", "abc123")
	if err != nil || found {
		t.Fatalf("missing file: found=%v err=%v, want found=false and no error", found, err)
	}
}

func TestGetCommitAndDefaultBranch(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + testRepo + "/git/commits/c1":
			io.WriteString(w, `{"sha":"c1","tree":{"sha":"t1"},"parents":[{"sha":"p1"},{"sha":"p2"}]}`)
		case "/repos/" + testRepo:
			io.WriteString(w, `{"full_name":"x","default_branch":"trunk"}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	cm, err := c.GetCommit(context.Background(), testRepo, "c1")
	if err != nil || cm.TreeSHA != "t1" || len(cm.Parents) != 2 || cm.Parents[0] != "p1" {
		t.Fatalf("commit = %+v err=%v", cm, err)
	}
	br, err := c.DefaultBranch(context.Background(), testRepo)
	if err != nil || br != "trunk" {
		t.Fatalf("default branch = %q err=%v", br, err)
	}
}

// TestCommitFiles: one commit through the git data API - a blob per written file, a
// tree over the parent's tree carrying a NULL sha for a deleted path (the only way
// the trees API removes a file), and a commit whose only parent is the parent.
func TestCommitFiles(t *testing.T) {
	var treeBody map[string]any
	var commitBody map[string]any
	blobs := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/f/r/git/commits/base":
			io.WriteString(w, `{"sha":"base","tree":{"sha":"basetree"},"parents":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/f/r/git/blobs":
			body := readJSON(t, r)
			if body["encoding"] != "base64" {
				t.Fatalf("blob encoding = %v", body["encoding"])
			}
			blobs++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"sha":"blob%d"}`, blobs)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/f/r/git/trees":
			treeBody = readJSON(t, r)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"sha":"newtree"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/f/r/git/commits":
			commitBody = readJSON(t, r)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"sha":"newcommit"}`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	sha, err := c.CommitFiles(context.Background(), "f/r", "base", "msg",
		map[string][]byte{"data/b.json": []byte("b"), "data/a.json": []byte("a")}, []string{"data/old.json"})
	if err != nil || sha != "newcommit" {
		t.Fatalf("CommitFiles = %q, %v", sha, err)
	}
	if blobs != 2 || treeBody["base_tree"] != "basetree" {
		t.Fatalf("blobs=%d tree=%v", blobs, treeBody)
	}
	entries := treeBody["tree"].([]any)
	if len(entries) != 3 {
		t.Fatalf("tree entries = %v", entries)
	}
	first := entries[0].(map[string]any)
	last := entries[2].(map[string]any)
	if first["path"] != "data/a.json" || first["sha"] != "blob1" {
		t.Fatalf("first entry = %v (want the sorted, blobbed a.json)", first)
	}
	if last["path"] != "data/old.json" || last["sha"] != nil {
		t.Fatalf("deletion entry = %v, want a null sha", last)
	}
	if v, ok := last["sha"]; !ok || v != nil {
		t.Fatalf("deletion must carry an explicit null sha: %v", last)
	}
	parents := commitBody["parents"].([]any)
	if commitBody["tree"] != "newtree" || len(parents) != 1 || parents[0] != "base" {
		t.Fatalf("commit body = %v", commitBody)
	}
}

func TestUpdateRef(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/f/r/git/refs/heads/sidecars/x-1" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body := readJSON(t, r)
		if body["sha"] != "s" || body["force"] != true {
			t.Fatalf("body = %v", body)
		}
		io.WriteString(w, `{}`)
	})
	if err := c.UpdateRef(context.Background(), "f/r", "sidecars/x-1", "s"); err != nil {
		t.Fatal(err)
	}
}

// TestTarballFollowsRedirectAndCaps: the tarball endpoint 302s to an archive host;
// the stream is handed to fn, and a body over the cap fails the read rather than
// being truncated silently.
func TestTarballFollowsRedirectAndCaps(t *testing.T) {
	var srvURL string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/f/r/tarball/abc":
			http.Redirect(w, r, srvURL+"/archive", http.StatusFound)
		case "/archive":
			io.WriteString(w, "0123456789")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srvURL = c.baseURL
	var got []byte
	if err := c.Tarball(context.Background(), "f/r", "abc", 10, func(r io.Reader) error {
		var err error
		got, err = io.ReadAll(r)
		return err
	}); err != nil || string(got) != "0123456789" {
		t.Fatalf("Tarball = %q, %v", got, err)
	}
	err := c.Tarball(context.Background(), "f/r", "abc", 9, func(r io.Reader) error {
		_, err := io.ReadAll(r)
		return err
	})
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("over-cap tarball err = %v, want errTooLarge", err)
	}
	if err := c.Tarball(context.Background(), "f/r", "missing", 10, func(io.Reader) error { return nil }); err == nil {
		t.Fatal("a 404 tarball must error")
	}
}

func TestIssueComments(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/issues/7/comments" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, `[{"body":"first","user":{"login":"a"}},{"body":"second","user":{"login":"github-actions[bot]"}}]`)
	})
	got, err := c.IssueComments(context.Background(), testRepo, 7)
	if err != nil || len(got) != 2 || got[1].Body != "second" || got[1].Author != "github-actions[bot]" {
		t.Fatalf("comments = %+v err=%v", got, err)
	}
}

// TestRateLimit: a 403 with x-ratelimit-remaining:0 becomes a *RateLimitError
// (and is not retried).
func TestRateLimit(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	})
	_, err := c.GetIssue(context.Background(), testRepo, 1)
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if rl.Status != http.StatusForbidden {
		t.Fatalf("rate-limit status = %d", rl.Status)
	}
	if calls != 1 {
		t.Fatalf("rate limit must not be retried, calls=%d", calls)
	}
	// A 403 that is NOT a rate limit (remaining header absent) is a plain APIError.
	c2 := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"forbidden"}`)
	})
	var api *APIError
	if err := func() error { _, e := c2.GetIssue(context.Background(), testRepo, 1); return e }(); !errors.As(err, &api) {
		t.Fatalf("non-rate-limit 403 err = %v, want *APIError", err)
	}
}

// TestRetryOn5xx: the first attempt 500s, the retry succeeds.
func TestRetryOn5xx(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"boom"}`)
			return
		}
		io.WriteString(w, `{"number":8,"html_url":"u","labels":[]}`)
	})
	iss, err := c.GetIssue(context.Background(), testRepo, 8)
	if err != nil {
		t.Fatalf("GetIssue after retry: %v", err)
	}
	if iss.Number != 8 {
		t.Fatalf("issue = %+v", iss)
	}
	if calls != 2 {
		t.Fatalf("expected exactly one retry, calls=%d", calls)
	}
}

// TestPersistent5xx: a 5xx on both attempts surfaces an *APIError (one retry
// only) carrying the status and trimmed body, never the token.
func TestPersistent5xx(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"message":"bad gateway"}`)
	})
	_, err := c.GetIssue(context.Background(), testRepo, 1)
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if api.Status != http.StatusBadGateway {
		t.Fatalf("status = %d", api.Status)
	}
	if calls != 2 {
		t.Fatalf("expected 1 retry then give up, calls=%d", calls)
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Fatalf("error leaked the token: %v", err)
	}
}

// TestUnauthenticatedClient: an empty token sends no Authorization header (the
// poller reads the public repo without a credential).
func TestUnauthenticatedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("unauthenticated client must send no Authorization header, got %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"number":1,"html_url":"u","state":"open","merged":false}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "")
	if _, err := c.GetPull(context.Background(), testRepo, 1); err != nil {
		t.Fatal(err)
	}
}
