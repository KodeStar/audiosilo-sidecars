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
type PR struct {
	Number int
	URL    string
	State  string
	Merged bool
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
		respBody, header, status, err := c.doOnce(ctx, method, path, payload)
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
func (c *Client) doOnce(ctx context.Context, method, path string, payload []byte) ([]byte, http.Header, int, error) {
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
	Number   int     `json:"number"`
	HTMLURL  string  `json:"html_url"`
	State    string  `json:"state"`
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
}

func (r pullResp) toPR() PR {
	return PR{
		Number: r.Number,
		URL:    r.HTMLURL,
		State:  r.State,
		// The list endpoint omits `merged`, so fall back to merged_at != null.
		Merged: r.Merged || r.MergedAt != nil,
	}
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

// PutContents creates or updates a file on branch at path with content (which is
// base64-encoded for the API). It first reads the file's existing blob sha on the
// branch and supplies it when present, so a resumed run (the file was committed on a
// prior attempt) UPDATES the file rather than 422ing on a create-over-existing-path; a
// 404 means the file is new and it is created without a sha.
func (c *Client) PutContents(ctx context.Context, repo, branch, path, message string, content []byte) error {
	sha, err := c.contentSHA(ctx, repo, branch, path)
	if err != nil {
		return err
	}
	reqBody := map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
	}
	if sha != "" {
		reqBody["sha"] = sha
	}
	_, _, err = c.request(ctx, http.MethodPut, "/repos/"+repo+"/contents/"+path, reqBody, http.StatusOK, http.StatusCreated)
	return err
}

// contentSHA returns the blob sha of a file on a branch, or "" when the file does not
// exist there (a 404). Any other error propagates.
func (c *Client) contentSHA(ctx context.Context, repo, branch, path string) (string, error) {
	respBody, _, err := c.request(ctx, http.MethodGet,
		"/repos/"+repo+"/contents/"+path+"?ref="+url.QueryEscape(branch), nil, http.StatusOK)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return "", nil
		}
		return "", err
	}
	var r struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return "", fmt.Errorf("contrib: decode contents: %w", err)
	}
	return r.SHA, nil
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

// PullFiles returns the file paths a pull request touches (used to read the real
// work slug out of a merged core PR's created work.json path).
func (c *Client) PullFiles(ctx context.Context, repo string, number int) ([]string, error) {
	respBody, _, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", repo, number), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var files []struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(respBody, &files); err != nil {
		return nil, fmt.Errorf("contrib: decode pull files: %w", err)
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Filename)
	}
	return out, nil
}
