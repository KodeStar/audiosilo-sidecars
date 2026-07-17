package contrib

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestPutContents(t *testing.T) {
	const contentsPath = "/repos/tester/audiosilo-meta/contents/data/works/aa/a-work/characters.json"

	// Create path: the file does not exist on the branch (GET 404), so PUT carries no sha.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == contentsPath:
			if got := r.URL.Query().Get("ref"); got != "sidecars/x-1" {
				t.Fatalf("ref = %q, want sidecars/x-1", got)
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut && r.URL.Path == contentsPath:
			body := readJSON(t, r)
			dec, err := base64.StdEncoding.DecodeString(body["content"].(string))
			if err != nil {
				t.Fatalf("content not base64: %v", err)
			}
			if string(dec) != `{"work":"a-work"}` {
				t.Fatalf("decoded content = %q", dec)
			}
			if body["branch"] != "sidecars/x-1" {
				t.Fatalf("branch = %v", body["branch"])
			}
			if _, ok := body["sha"]; ok {
				t.Fatalf("a create over a missing path must not carry a sha: %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"content":{"sha":"abc"}}`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	if err := c.PutContents(context.Background(), "tester/audiosilo-meta", "sidecars/x-1",
		contentsPath[len("/repos/tester/audiosilo-meta/contents/"):], "add characters", []byte(`{"work":"a-work"}`)); err != nil {
		t.Fatalf("PutContents (create): %v", err)
	}

	// Update path (resume): the file already exists (GET returns its sha), so the PUT must
	// carry that sha - otherwise GitHub 422s the create-over-existing-path.
	c2 := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			io.WriteString(w, `{"sha":"existing-sha"}`)
		case http.MethodPut:
			body := readJSON(t, r)
			if body["sha"] != "existing-sha" {
				t.Fatalf("an update over an existing path must carry its sha, got %v", body["sha"])
			}
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{}`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	if err := c2.PutContents(context.Background(), "tester/audiosilo-meta", "sidecars/x-1",
		contentsPath[len("/repos/tester/audiosilo-meta/contents/"):], "update", []byte(`{"work":"a-work"}`)); err != nil {
		t.Fatalf("PutContents (update): %v", err)
	}
}

// TestBranchRef: an existing branch returns its sha + exists=true; a 404 returns
// exists=false with no error (a resume then reuses/creates as appropriate).
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
		io.WriteString(w, `{"number":123,"html_url":"u","state":"closed","merged":true}`)
	})
	pr, err := c.GetPull(context.Background(), testRepo, 123)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Merged || pr.State != "closed" {
		t.Fatalf("pr = %+v", pr)
	}
}

func TestPullFiles(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/pulls/123/files" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, `[{"filename":"data/works/th/the-book/work.json"},{"filename":"data/people/au/author.json"}]`)
	})
	files, err := c.PullFiles(context.Background(), testRepo, 123)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] != "data/works/th/the-book/work.json" {
		t.Fatalf("files = %v", files)
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
