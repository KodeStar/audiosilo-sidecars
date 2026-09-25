package contrib

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		io.WriteString(w, `{"number":123,"html_url":"u","state":"closed","merged":true,"base":{"sha":"b4se"},"head":{"sha":"h3ad"}}`)
	})
	pr, err := c.GetPull(context.Background(), testRepo, 123)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Merged || pr.State != "closed" || pr.BaseSHA != "b4se" || pr.HeadSHA != "h3ad" {
		t.Fatalf("pr = %+v", pr)
	}
}

func TestCompare(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/compare/b4se...h3ad" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, `{"merge_base_commit":{"sha":"mb"},"files":[{"filename":"data/works/c/cat.json","status":"modified"},`+
			`{"filename":"data/works/c/dog.json","status":"renamed","previous_filename":"data/works/c/cow.json"}]}`)
	})
	got, err := c.Compare(context.Background(), testRepo, "b4se", "h3ad")
	if err != nil {
		t.Fatal(err)
	}
	want := []ChangedFile{
		{Filename: "data/works/c/cat.json"},
		{Filename: "data/works/c/dog.json", PreviousFilename: "data/works/c/cow.json"},
	}
	if got.MergeBase != "mb" || len(got.Files) != 2 || got.Files[0] != want[0] || got.Files[1] != want[1] {
		t.Fatalf("compare = %+v", got)
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
