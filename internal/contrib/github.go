package contrib

import (
	"bytes"
	"context"
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

// Issue is the subset of a GitHub issue the contribution flow needs. Labels is
// the labels that actually stuck (GitHub silently drops labels set by a
// non-collaborator, so the caller re-reads via GetIssue to verify).
type Issue struct {
	Number    int
	URL       string
	State     string // "open" | "closed" (the poller closes an issue with no merged intake PR)
	Labels    []string
	UpdatedAt string // GitHub's updated_at: moves on any edit, comment or label change
}

// PR is the subset of a GitHub pull request the contribution flow needs. BaseSHA
// and HeadSHA bound the PR's own change (the poller diffs them to learn which work
// a merged add-work PR created, whatever the merge style).
type PR struct {
	Number  int
	URL     string
	State   string
	Merged  bool
	BaseSHA string
	HeadSHA string
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
	Number    int    `json:"number"`
	HTMLURL   string `json:"html_url"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (r issueResp) toIssue() Issue {
	labels := make([]string, 0, len(r.Labels))
	for _, l := range r.Labels {
		labels = append(labels, l.Name)
	}
	return Issue{Number: r.Number, URL: r.HTMLURL, State: r.State, Labels: labels, UpdatedAt: r.UpdatedAt}
}

type pullResp struct {
	Number   int     `json:"number"`
	HTMLURL  string  `json:"html_url"`
	State    string  `json:"state"`
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
	Base     struct {
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (r pullResp) toPR() PR {
	return PR{
		Number: r.Number,
		URL:    r.HTMLURL,
		State:  r.State,
		// The list endpoint omits `merged`, so fall back to merged_at != null.
		Merged:  r.Merged || r.MergedAt != nil,
		BaseSHA: r.Base.SHA,
		HeadSHA: r.Head.SHA,
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

// ChangedFile is one file a comparison touches. PreviousFilename is set for a
// rename (a pack REBIND renames the file, since a pack's bound is its name).
type ChangedFile struct {
	Filename         string
	PreviousFilename string
}

// Comparison is GitHub's three-dot compare of base...head: the merge base and the
// files changed from it to head.
type Comparison struct {
	MergeBase string
	Files     []ChangedFile
}

// Compare compares base...head (GitHub lists at most 300 files per comparison).
func (c *Client) Compare(ctx context.Context, repo, base, head string) (Comparison, error) {
	respBody, _, err := c.request(ctx, http.MethodGet,
		"/repos/"+repo+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head), nil, http.StatusOK)
	if err != nil {
		return Comparison{}, err
	}
	var r struct {
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return Comparison{}, fmt.Errorf("contrib: decode compare: %w", err)
	}
	out := Comparison{MergeBase: r.MergeBase.SHA}
	for _, f := range r.Files {
		out.Files = append(out.Files, ChangedFile{Filename: f.Filename, PreviousFilename: f.PreviousFilename})
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
