package contrib

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// requestTimeout bounds a single REST request (a poll of many small requests
// wraps this in its own overall context).
const requestTimeout = 15 * time.Second

// maxBodyBytes caps how much of a response body is read - GitHub JSON for the
// endpoints we use is small, and this bounds memory against a pathological body.
const maxBodyBytes = 8 << 20 // 8 MiB

// maxErrorBody caps the response snippet embedded in an APIError/RateLimitError.
const maxErrorBody = 300

// forkPollTimeout / forkPollInterval bound the EnsureFork readiness poll. The
// interval is a var so tests can shrink it.
const forkPollTimeout = 30 * time.Second

var forkPollInterval = 2 * time.Second

// Issue is the subset of a GitHub issue the contribution flow needs. Labels is
// the labels that actually stuck (GitHub silently drops labels set by a
// non-collaborator, so the caller re-reads via GetIssue to verify).
type Issue struct {
	Number int
	URL    string
	State  string // "open" | "closed" (the poller closes an issue with no merged intake PR)
	Labels []string
}

// PR is the subset of a GitHub pull request the contribution flow needs.
// MergeCommitSHA is the commit a merge left on the base branch (set once the PR
// merged, in every merge mode) and Commits the number of commits the PR carried:
// together they locate the PR's net change on the base branch, which is how the
// poller learns which work a merged add-work PR created.
type PR struct {
	Number         int
	URL            string
	State          string
	Merged         bool
	MergeCommitSHA string
	Commits        int
}

// APIError is an unexpected (non-success) GitHub HTTP response. It carries the
// status and a trimmed response body; it NEVER carries the request's bearer
// token (that lives only in the request header, and GitHub does not echo it).
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github api: status %d: %s", e.Status, e.Body)
}

// RateLimitError is a 403/429 response whose x-ratelimit-remaining is 0. Callers
// treat it as transient (retry later), distinct from a permanent APIError.
type RateLimitError struct {
	Status int
	Body   string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github api rate limited: status %d: %s", e.Status, e.Body)
}

// Client is a minimal GitHub REST client over the stdlib HTTP client. baseURL is
// injectable so tests point it at an httptest server. token may be empty (public
// reads work unauthenticated).
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient returns a Client for baseURL (e.g. https://api.github.com) carrying
// token as a Bearer credential (empty token = unauthenticated).
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{},
	}
}

// request performs one REST call with a per-request timeout and a single retry
// on a 5xx. A 403/429 with x-ratelimit-remaining:0 becomes a *RateLimitError; a
// non-want status becomes an *APIError. want lists the acceptable status codes
// (empty = any 2xx).
func (c *Client) request(ctx context.Context, method, path string, body any, want ...int) ([]byte, http.Header, error) {
	return c.requestAccept(ctx, "", method, path, body, want...)
}

// requestAccept is request with an explicit Accept media type ("" = the default
// application/vnd.github+json), for the endpoints that answer raw bytes.
func (c *Client) requestAccept(ctx context.Context, accept, method, path string, body any, want ...int) ([]byte, http.Header, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("contrib: marshal request body: %w", err)
		}
	}

	var lastErr error
	for attempt := range 2 {
		respBody, header, status, err := c.doOnce(ctx, accept, method, path, payload)
		if err != nil {
			return nil, nil, err
		}
		if rl := rateLimitError(status, header, respBody); rl != nil {
			return nil, header, rl
		}
		if status >= 500 && attempt == 0 {
			lastErr = newAPIError(status, respBody)
			continue // retry once on a server error
		}
		if !statusWanted(status, want) {
			return nil, header, newAPIError(status, respBody)
		}
		return respBody, header, nil
	}
	return nil, nil, lastErr
}

// doOnce issues a single HTTP request and reads the (bounded) response body.
func (c *Client) doOnce(ctx context.Context, accept, method, path string, payload []byte) ([]byte, http.Header, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("contrib: new request: %w", err)
	}
	c.setHeaders(req, payload != nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("contrib: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("contrib: read response: %w", err)
	}
	return respBody, resp.Header, resp.StatusCode, nil
}

// setHeaders applies the GitHub API headers and (when present) the bearer token.
func (c *Client) setHeaders(req *http.Request, hasBody bool) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "audiosilo-sidecars")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
}

// statusWanted reports whether status is acceptable: any 2xx when want is empty,
// otherwise membership in want.
func statusWanted(status int, want []int) bool {
	if len(want) == 0 {
		return status >= 200 && status < 300
	}
	return slices.Contains(want, status)
}

// rateLimitError returns a *RateLimitError when status is 403/429 and the
// x-ratelimit-remaining header is 0, else nil.
func rateLimitError(status int, header http.Header, body []byte) *RateLimitError {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return nil
	}
	if header.Get("X-RateLimit-Remaining") != "0" {
		return nil
	}
	return &RateLimitError{Status: status, Body: trimBody(body)}
}

func newAPIError(status int, body []byte) *APIError {
	return &APIError{Status: status, Body: trimBody(body)}
}

// trimBody trims whitespace and caps the body snippet used in errors.
func trimBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody]
	}
	return s
}

// OwnerOf returns the owner segment of an "owner/name" repo (or fork full name),
// or the whole string when it has no slash. Exported so the contributing stage
// shares this one parser rather than duplicating it.
func OwnerOf(repo string) string {
	if i := strings.IndexByte(repo, '/'); i >= 0 {
		return repo[:i]
	}
	return repo
}

// --- response shapes ---

type issueResp struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (r issueResp) toIssue() Issue {
	labels := make([]string, 0, len(r.Labels))
	for _, l := range r.Labels {
		labels = append(labels, l.Name)
	}
	return Issue{Number: r.Number, URL: r.HTMLURL, State: r.State, Labels: labels}
}

type pullResp struct {
	Number         int     `json:"number"`
	HTMLURL        string  `json:"html_url"`
	State          string  `json:"state"`
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Commits        int     `json:"commits"`
}

func (r pullResp) toPR() PR {
	pr := PR{
		Number: r.Number,
		URL:    r.HTMLURL,
		State:  r.State,
		// The list endpoint omits `merged`, so fall back to merged_at != null.
		Merged:  r.Merged || r.MergedAt != nil,
		Commits: r.Commits,
	}
	// merge_commit_sha is GitHub's test-merge commit while a PR is open; it only
	// names the commit on the base branch once the PR has merged.
	if pr.Merged {
		pr.MergeCommitSHA = r.MergeCommitSHA
	}
	return pr
}

// --- methods ---

// CreateIssue opens an issue on repo with the given labels, returning the issue
// as GitHub echoed it. GitHub may silently drop labels for a non-collaborator,
// so the caller re-reads via GetIssue to see which labels actually stuck.
func (c *Client) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (Issue, error) {
	reqBody := map[string]any{"title": title, "body": body, "labels": labels}
	respBody, _, err := c.request(ctx, http.MethodPost, "/repos/"+repo+"/issues", reqBody, http.StatusCreated)
	if err != nil {
		return Issue{}, err
	}
	var r issueResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		return Issue{}, fmt.Errorf("contrib: decode issue: %w", err)
	}
	return r.toIssue(), nil
}

// GetIssue reads a single issue back (used to verify labels stuck).
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	respBody, _, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, http.StatusOK)
	if err != nil {
		return Issue{}, err
	}
	var r issueResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		return Issue{}, fmt.Errorf("contrib: decode issue: %w", err)
	}
	return r.toIssue(), nil
}

// CreateGist uploads files (filename -> content) as a gist and returns their raw
// URLs (filename -> raw_url). secret=true makes the gist unlisted.
func (c *Client) CreateGist(ctx context.Context, files map[string]string, secret bool) (map[string]string, error) {
	fm := make(map[string]map[string]string, len(files))
	for name, content := range files {
		fm[name] = map[string]string{"content": content}
	}
	reqBody := map[string]any{"public": !secret, "files": fm}
	respBody, _, err := c.request(ctx, http.MethodPost, "/gists", reqBody, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	var r struct {
		Files map[string]struct {
			RawURL string `json:"raw_url"`
		} `json:"files"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return nil, fmt.Errorf("contrib: decode gist: %w", err)
	}
	out := make(map[string]string, len(r.Files))
	for name, f := range r.Files {
		out[name] = f.RawURL
	}
	return out, nil
}

// EnsureFork forks repo into the authenticated user's account (a no-op if it
// already exists) and polls until the fork is queryable, returning its
// "owner/name". The poll is capped at forkPollTimeout.
func (c *Client) EnsureFork(ctx context.Context, repo string) (string, error) {
	respBody, _, err := c.request(ctx, http.MethodPost, "/repos/"+repo+"/forks", nil, http.StatusAccepted, http.StatusOK)
	if err != nil {
		return "", err
	}
	var r struct {
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return "", fmt.Errorf("contrib: decode fork: %w", err)
	}
	if r.FullName == "" {
		return "", errors.New("contrib: fork response missing full_name")
	}

	deadline := time.Now().Add(forkPollTimeout)
	for {
		_, _, gerr := c.request(ctx, http.MethodGet, "/repos/"+r.FullName, nil, http.StatusOK)
		if gerr == nil {
			return r.FullName, nil
		}
		var apiErr *APIError
		if !errors.As(gerr, &apiErr) || apiErr.Status != http.StatusNotFound {
			// A rate limit, transport failure, or other status is terminal.
			return "", gerr
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("contrib: fork %s not ready after %s", r.FullName, forkPollTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(forkPollInterval):
		}
	}
}

// MergeUpstream fast-forwards the fork's branch from its upstream (best-effort:
// callers ignore a returned error and proceed to branch anyway).
func (c *Client) MergeUpstream(ctx context.Context, fork, branch string) error {
	_, _, err := c.request(ctx, http.MethodPost, "/repos/"+fork+"/merge-upstream", map[string]string{"branch": branch}, http.StatusOK)
	return err
}

// BranchRef returns a branch's head commit SHA and whether the branch exists (a 404
// -> exists=false with a nil error). A resumed submit uses it to reuse an
// already-created branch instead of re-creating it (which 422s "reference already
// exists"). Any non-404 error propagates.
func (c *Client) BranchRef(ctx context.Context, repo, branch string) (sha string, exists bool, err error) {
	respBody, _, rerr := c.request(ctx, http.MethodGet, "/repos/"+repo+"/git/ref/heads/"+branch, nil, http.StatusOK)
	if rerr != nil {
		var apiErr *APIError
		if errors.As(rerr, &apiErr) && apiErr.Status == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, rerr
	}
	var r struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return "", false, fmt.Errorf("contrib: decode ref: %w", err)
	}
	return r.Object.SHA, true, nil
}

// FindOpenPRByHead returns the open PR whose head branch is head ("owner:branch") and
// whether one exists, so a resumed submit reuses an already-open PR rather than 422ing
// on a duplicate. found is false when no open PR has that head.
func (c *Client) FindOpenPRByHead(ctx context.Context, repo, head string) (PR, bool, error) {
	q := url.Values{"head": {head}, "state": {"open"}}
	respBody, _, err := c.request(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, http.StatusOK)
	if err != nil {
		return PR{}, false, err
	}
	var list []pullResp
	if err := json.Unmarshal(respBody, &list); err != nil {
		return PR{}, false, fmt.Errorf("contrib: decode pulls: %w", err)
	}
	if len(list) == 0 {
		return PR{}, false, nil
	}
	return list[0].toPR(), true, nil
}

// BranchSHA returns the head commit SHA of a branch.
func (c *Client) BranchSHA(ctx context.Context, repo, branch string) (string, error) {
	respBody, _, err := c.request(ctx, http.MethodGet, "/repos/"+repo+"/git/ref/heads/"+branch, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var r struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return "", fmt.Errorf("contrib: decode ref: %w", err)
	}
	return r.Object.SHA, nil
}

// CreateRef creates a git ref (e.g. refs/heads/<branch>) at sha.
func (c *Client) CreateRef(ctx context.Context, repo, ref, sha string) error {
	_, _, err := c.request(ctx, http.MethodPost, "/repos/"+repo+"/git/refs", map[string]string{"ref": ref, "sha": sha}, http.StatusCreated)
	return err
}

// CreatePull opens a pull request from head into base on repo.
func (c *Client) CreatePull(ctx context.Context, repo, head, base, title, body string) (PR, error) {
	reqBody := map[string]string{"title": title, "head": head, "base": base, "body": body}
	respBody, _, err := c.request(ctx, http.MethodPost, "/repos/"+repo+"/pulls", reqBody, http.StatusCreated)
	if err != nil {
		return PR{}, err
	}
	var r pullResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		return PR{}, fmt.Errorf("contrib: decode pull: %w", err)
	}
	return r.toPR(), nil
}

// FindIntakePR looks up the intake bot's PR for an issue: the branch
// intake/issue-<n> in the upstream repo. found is false when no such PR exists.
func (c *Client) FindIntakePR(ctx context.Context, repo string, issueNumber int) (PR, bool, error) {
	head := fmt.Sprintf("%s:intake/issue-%d", OwnerOf(repo), issueNumber)
	q := url.Values{"head": {head}, "state": {"all"}}
	respBody, _, err := c.request(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, http.StatusOK)
	if err != nil {
		return PR{}, false, err
	}
	var list []pullResp
	if err := json.Unmarshal(respBody, &list); err != nil {
		return PR{}, false, fmt.Errorf("contrib: decode pulls: %w", err)
	}
	if len(list) == 0 {
		return PR{}, false, nil
	}
	return list[0].toPR(), true, nil
}

// GetPull reads a single pull request (its state and merged flag).
func (c *Client) GetPull(ctx context.Context, repo string, number int) (PR, error) {
	respBody, _, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, http.StatusOK)
	if err != nil {
		return PR{}, err
	}
	var r pullResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		return PR{}, fmt.Errorf("contrib: decode pull: %w", err)
	}
	return r.toPR(), nil
}

// PullFile is one file a change touches. Status is GitHub's
// added|removed|modified|renamed|copied|changed|unchanged; PreviousFilename is set
// for a rename (a pack REBIND renames the file, since a pack's bound is its name).
type PullFile struct {
	Filename         string
	Status           string
	PreviousFilename string
}

// CompareFiles returns the files changed between two commits (GitHub's compare
// API, base...head; with base an ancestor of head that is exactly base..head). It
// is how the poller reads a merged PR's net change on the base branch, whatever
// the merge style. GitHub lists at most 300 files per comparison.
func (c *Client) CompareFiles(ctx context.Context, repo, base, head string) ([]PullFile, error) {
	respBody, _, err := c.request(ctx, http.MethodGet,
		"/repos/"+repo+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var r struct {
		Files []struct {
			Filename         string `json:"filename"`
			Status           string `json:"status"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return nil, fmt.Errorf("contrib: decode compare: %w", err)
	}
	out := make([]PullFile, 0, len(r.Files))
	for _, f := range r.Files {
		out = append(out, PullFile{Filename: f.Filename, Status: f.Status, PreviousFilename: f.PreviousFilename})
	}
	return out, nil
}

// maxPullCommitPages bounds PullCommits' pagination: GitHub lists at most 250
// commits for a pull request.
const maxPullCommitPages = 3

// PullCommits returns a pull request's commits, oldest first, with the message and
// author date of each - what a rebase merge preserves on the commits it re-creates.
func (c *Client) PullCommits(ctx context.Context, repo string, number int) ([]Commit, error) {
	var out []Commit
	for page := 1; page <= maxPullCommitPages; page++ {
		respBody, _, err := c.request(ctx, http.MethodGet,
			fmt.Sprintf("/repos/%s/pulls/%d/commits?per_page=100&page=%d", repo, number, page), nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var list []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Date string `json:"date"`
				} `json:"author"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(respBody, &list); err != nil {
			return nil, fmt.Errorf("contrib: decode pull commits: %w", err)
		}
		for _, cm := range list {
			out = append(out, Commit{SHA: cm.SHA, Message: cm.Commit.Message, AuthorDate: cm.Commit.Author.Date})
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}

// FileAt reads a file's raw bytes at ref (a commit sha or branch). found is false
// for a 404 - the file does not exist at that revision - with a nil error. The raw
// media type is used so files over the contents API's 1 MB JSON limit still read.
func (c *Client) FileAt(ctx context.Context, repo, path, ref string) (content []byte, found bool, err error) {
	respBody, _, rerr := c.requestAccept(ctx, "application/vnd.github.raw+json", http.MethodGet,
		"/repos/"+repo+"/contents/"+escapePath(path)+"?ref="+url.QueryEscape(ref), nil, http.StatusOK)
	if rerr != nil {
		var apiErr *APIError
		if errors.As(rerr, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, rerr
	}
	return respBody, true, nil
}

// escapePath escapes each segment of a slash-separated repository path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// Commit is the subset of a git commit object the contribution flow needs.
type Commit struct {
	SHA        string
	TreeSHA    string
	Parents    []string
	Message    string
	AuthorDate string
}

// GetCommit reads a git commit object (its tree and parents).
func (c *Client) GetCommit(ctx context.Context, repo, sha string) (Commit, error) {
	respBody, _, err := c.request(ctx, http.MethodGet, "/repos/"+repo+"/git/commits/"+url.PathEscape(sha), nil, http.StatusOK)
	if err != nil {
		return Commit{}, err
	}
	var r struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Message string `json:"message"`
		Author  struct {
			Date string `json:"date"`
		} `json:"author"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return Commit{}, fmt.Errorf("contrib: decode commit: %w", err)
	}
	out := Commit{SHA: r.SHA, TreeSHA: r.Tree.SHA, Message: r.Message, AuthorDate: r.Author.Date}
	for _, p := range r.Parents {
		out.Parents = append(out.Parents, p.SHA)
	}
	return out, nil
}

// DefaultBranch returns a repository's default branch name.
func (c *Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	respBody, _, err := c.request(ctx, http.MethodGet, "/repos/"+repo, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return "", fmt.Errorf("contrib: decode repo: %w", err)
	}
	if r.DefaultBranch == "" {
		return "", errors.New("contrib: repo response missing default_branch")
	}
	return r.DefaultBranch, nil
}

// CommitFiles writes ONE commit on top of parent in repo through the git data API:
// a blob per written file, a tree over parent's tree (a deleted path is an entry
// with a null sha), and a commit whose only parent is parent. It returns the new
// commit's sha; moving a branch onto it is the caller's (CreateRef / UpdateRef).
// Paths are repository-relative. One commit rather than a contents-API call per
// file, so a pack split (files added, one removed) lands atomically.
func (c *Client) CommitFiles(ctx context.Context, repo, parent, message string, files map[string][]byte, deleted []string) (string, error) {
	base, err := c.GetCommit(ctx, repo, parent)
	if err != nil {
		return "", err
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	type treeEntry struct {
		Path string  `json:"path"`
		Mode string  `json:"mode"`
		Type string  `json:"type"`
		SHA  *string `json:"sha"`
	}
	entries := make([]treeEntry, 0, len(files)+len(deleted))
	for _, p := range paths {
		blobBody := map[string]string{"content": base64.StdEncoding.EncodeToString(files[p]), "encoding": "base64"}
		respBody, _, err := c.request(ctx, http.MethodPost, "/repos/"+repo+"/git/blobs", blobBody, http.StatusCreated)
		if err != nil {
			return "", err
		}
		var blob struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal(respBody, &blob); err != nil || blob.SHA == "" {
			return "", fmt.Errorf("contrib: decode blob for %s: %v", p, err)
		}
		sha := blob.SHA
		entries = append(entries, treeEntry{Path: p, Mode: "100644", Type: "blob", SHA: &sha})
	}
	for _, p := range deleted {
		entries = append(entries, treeEntry{Path: p, Mode: "100644", Type: "blob", SHA: nil})
	}
	respBody, _, err := c.request(ctx, http.MethodPost, "/repos/"+repo+"/git/trees",
		map[string]any{"base_tree": base.TreeSHA, "tree": entries}, http.StatusCreated)
	if err != nil {
		return "", err
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(respBody, &tree); err != nil || tree.SHA == "" {
		return "", fmt.Errorf("contrib: decode tree: %v", err)
	}
	respBody, _, err = c.request(ctx, http.MethodPost, "/repos/"+repo+"/git/commits",
		map[string]any{"message": message, "tree": tree.SHA, "parents": []string{parent}}, http.StatusCreated)
	if err != nil {
		return "", err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(respBody, &commit); err != nil || commit.SHA == "" {
		return "", fmt.Errorf("contrib: decode commit: %v", err)
	}
	return commit.SHA, nil
}

// UpdateRef force-moves an existing branch to sha (a resumed PR-mode submit
// rebuilds its one commit on a fresh base rather than stacking on a stale one).
func (c *Client) UpdateRef(ctx context.Context, repo, branch, sha string) error {
	_, _, err := c.request(ctx, http.MethodPatch, "/repos/"+repo+"/git/refs/heads/"+branch,
		map[string]any{"sha": sha, "force": true}, http.StatusOK)
	return err
}

// tarballTimeout bounds a repository tarball download - far longer than one REST
// call, since the archive is megabytes.
const tarballTimeout = 5 * time.Minute

// Tarball streams repo's gzipped tarball at ref to fn. GitHub answers with a
// redirect to its archive host, which the client follows (net/http drops the
// Authorization header on the cross-host hop). The body is capped at maxBytes; a
// longer archive fails fn's read rather than being read whole.
func (c *Client) Tarball(ctx context.Context, repo, ref string, maxBytes int64, fn func(io.Reader) error) error {
	reqCtx, cancel := context.WithTimeout(ctx, tarballTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.baseURL+"/repos/"+repo+"/tarball/"+url.PathEscape(ref), nil)
	if err != nil {
		return fmt.Errorf("contrib: new request: %w", err)
	}
	c.setHeaders(req, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("contrib: GET tarball %s@%s: %w", repo, ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		if rl := rateLimitError(resp.StatusCode, resp.Header, body); rl != nil {
			return rl
		}
		return newAPIError(resp.StatusCode, body)
	}
	return fn(&cappedReader{r: resp.Body, left: maxBytes})
}

// errTooLarge is returned by a cappedReader once its cap is exceeded.
var errTooLarge = errors.New("contrib: download exceeds its size cap")

// cappedReader fails (rather than silently truncating, as io.LimitReader would)
// once more than left bytes have been read.
type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, errTooLarge
	}
	return n, err
}

// IssueComment is one comment on an issue.
type IssueComment struct {
	Body   string
	Author string
}

// maxCommentPages bounds IssueComments' pagination.
const maxCommentPages = 10

// IssueComments lists an issue's comments, oldest first (the intake poller reads
// the bot's latest verdict comment from it).
func (c *Client) IssueComments(ctx context.Context, repo string, number int) ([]IssueComment, error) {
	var out []IssueComment
	for page := 1; page <= maxCommentPages; page++ {
		respBody, _, err := c.request(ctx, http.MethodGet,
			fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", repo, number, page), nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var list []struct {
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(respBody, &list); err != nil {
			return nil, fmt.Errorf("contrib: decode comments: %w", err)
		}
		for _, cm := range list {
			out = append(out, IssueComment{Body: cm.Body, Author: cm.User.Login})
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}
