// Package metaops talks to the community metadata API (meta.audiosilo.app): it
// resolves a book's identity to a work and reports which expressive-layer
// sidecars (characters/recaps) that work already has, and it wraps the
// audiosilo-meta folder scanner (pkg/scan) as an async job. It uses ONLY the Go
// stdlib HTTP client and the meta module - no scraping. Every network path
// degrades gracefully: a down or absent metadata service marks coverage
// "unavailable" and never fails a scan.
package metaops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// coverageTTL bounds how long coverage/lookup/work-detail responses are cached.
const coverageTTL = time.Hour

// httpTimeout bounds a single metadata request.
const httpTimeout = 15 * time.Second

// Coverage is the per-book coverage verdict merged into scan results and stored
// on a book. It answers the two questions the Library UI asks: is this a known
// work, and which sidecars does it still need.
type Coverage struct {
	// Available is false when the metadata service is disabled (no base URL) or
	// unreachable; the UI then shows "coverage unavailable" and the book stays
	// selectable. Known/HasCharacters/HasRecaps are meaningful only when true.
	Available bool `json:"available"`
	// Known is true when the book's asin/isbn resolved to a work in the DB. False
	// means no identity or a lookup miss - the UI shows "unknown".
	Known bool `json:"known"`
	// WorkID is the resolved work id (empty when not Known).
	WorkID string `json:"work_id,omitempty"`
	// HasCharacters / HasRecaps report whether the work already carries that
	// sidecar (so it does not need contributing).
	HasCharacters bool `json:"has_characters"`
	HasRecaps     bool `json:"has_recaps"`
}

// Client is the metadata API client with an in-memory TTL cache.
type Client struct {
	baseURL string
	http    *http.Client
	now     func() time.Time

	mu       sync.Mutex
	coverage *coverageCacheEntry
	lookups  map[string]*lookupCacheEntry // key: "asin:<v>" | "isbn:<v>"
	works    map[string]*workCacheEntry   // key: work id
}

type coverageCacheEntry struct {
	at      time.Time
	index   map[string]map[string]bool // workID -> {dimension -> missing}
	present bool                       // coverage response carried a missing[] list
}

type lookupCacheEntry struct {
	at     time.Time
	workID string
	found  bool
}

type workCacheEntry struct {
	at       time.Time
	hasChars bool
	hasRecap bool
}

// NewClient returns a metadata client for baseURL. An empty baseURL yields a
// client whose CoverageFor always reports Available=false (metadata disabled).
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: httpTimeout},
		now:     time.Now,
		lookups: map[string]*lookupCacheEntry{},
		works:   map[string]*workCacheEntry{},
	}
}

// Enabled reports whether the client has a metadata base URL to query.
func (c *Client) Enabled() bool { return c.baseURL != "" }

// CoverageFor resolves (asin, isbn) to a coverage verdict. It never returns an
// error for a network/upstream problem - those degrade to Available=false so the
// caller (the scan) proceeds. An error is returned only for a cancelled context.
func (c *Client) CoverageFor(ctx context.Context, asin, isbn string) (Coverage, error) {
	if !c.Enabled() {
		return Coverage{Available: false}, nil
	}
	asin, isbn = strings.TrimSpace(asin), strings.TrimSpace(isbn)
	if asin == "" && isbn == "" {
		// Configured and reachable in principle, but this book has no identity.
		return Coverage{Available: true, Known: false}, nil
	}

	workID, found, ok := c.lookup(ctx, asin, isbn)
	if err := ctx.Err(); err != nil {
		return Coverage{}, err
	}
	if !ok {
		return Coverage{Available: false}, nil // upstream unreachable
	}
	if !found {
		return Coverage{Available: true, Known: false}, nil
	}

	cov := Coverage{Available: true, Known: true, WorkID: workID}

	// Prefer the bulk coverage feed's missing[] map; fall back to the per-work
	// detail when the deployed server omits it (older metaserve).
	if entry, present, ok := c.coverageIndex(ctx); ok && present {
		miss := entry[workID]
		cov.HasCharacters = !miss["characters"]
		cov.HasRecaps = !miss["recaps"]
		return cov, nil
	}
	if hasChars, hasRecap, ok := c.workDetail(ctx, workID); ok {
		cov.HasCharacters = hasChars
		cov.HasRecaps = hasRecap
	}
	// If the dimension probe also failed, leave has_* false (work exists,
	// coverage of its dimensions is simply unknown this pass).
	return cov, nil
}

// getJSON performs a GET and decodes JSON into v. It returns (found=false) for a
// 404 and (ok=false) for any transport/decoding/non-2xx failure.
func (c *Client) getJSON(ctx context.Context, path string, v any) (found, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return false, false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, true
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return false, false
	}
	if err := json.Unmarshal(body, v); err != nil {
		return false, false
	}
	return true, true
}

// lookup resolves an identity to a work id, cached per identity. ok=false means
// the upstream was unreachable; found=false means a clean miss (no such work).
func (c *Client) lookup(ctx context.Context, asin, isbn string) (workID string, found, ok bool) {
	key := "isbn:" + isbn
	q := url.Values{}
	if asin != "" {
		key = "asin:" + asin
		q.Set("asin", asin)
	} else {
		q.Set("isbn", isbn)
	}
	c.mu.Lock()
	if e, hit := c.lookups[key]; hit && c.now().Sub(e.at) < coverageTTL {
		c.mu.Unlock()
		return e.workID, e.found, true
	}
	c.mu.Unlock()

	var res struct {
		Work *struct {
			ID string `json:"id"`
		} `json:"work"`
	}
	f, okc := c.getJSON(ctx, "/api/v1/lookup?"+q.Encode(), &res)
	if !okc {
		return "", false, false
	}
	entry := &lookupCacheEntry{at: c.now()}
	if f && res.Work != nil {
		entry.workID = res.Work.ID
		entry.found = true
	}
	c.mu.Lock()
	c.lookups[key] = entry
	c.mu.Unlock()
	return entry.workID, entry.found, true
}

// coverageIndex fetches (and caches) the bulk coverage feed. present reports
// whether the response carried a missing[] list (older servers omit it).
func (c *Client) coverageIndex(ctx context.Context) (index map[string]map[string]bool, present, ok bool) {
	c.mu.Lock()
	if c.coverage != nil && c.now().Sub(c.coverage.at) < coverageTTL {
		e := c.coverage
		c.mu.Unlock()
		return e.index, e.present, true
	}
	c.mu.Unlock()

	var res struct {
		Missing *[]struct {
			ID      string   `json:"id"`
			Missing []string `json:"missing"`
		} `json:"missing"`
	}
	_, okc := c.getJSON(ctx, "/api/v1/coverage", &res)
	if !okc {
		return nil, false, false
	}
	entry := &coverageCacheEntry{at: c.now(), index: map[string]map[string]bool{}}
	if res.Missing != nil {
		entry.present = true
		for _, w := range *res.Missing {
			dims := map[string]bool{}
			for _, d := range w.Missing {
				dims[d] = true
			}
			entry.index[w.ID] = dims
		}
	}
	c.mu.Lock()
	c.coverage = entry
	c.mu.Unlock()
	return entry.index, entry.present, true
}

// workDetail fetches per-work sidecar presence, cached per work. Used only when
// the coverage feed lacks a missing[] list. Sidecar keys are omitempty on the
// wire, so a present, non-empty array means the work carries that sidecar.
func (c *Client) workDetail(ctx context.Context, workID string) (hasChars, hasRecap, ok bool) {
	c.mu.Lock()
	if e, hit := c.works[workID]; hit && c.now().Sub(e.at) < coverageTTL {
		c.mu.Unlock()
		return e.hasChars, e.hasRecap, true
	}
	c.mu.Unlock()

	var res struct {
		Characters []json.RawMessage `json:"characters"`
		Recaps     []json.RawMessage `json:"recaps"`
	}
	found, okc := c.getJSON(ctx, "/api/v1/works/"+url.PathEscape(workID), &res)
	if !okc || !found {
		return false, false, false
	}
	entry := &workCacheEntry{at: c.now(), hasChars: len(res.Characters) > 0, hasRecap: len(res.Recaps) > 0}
	c.mu.Lock()
	c.works[workID] = entry
	c.mu.Unlock()
	return entry.hasChars, entry.hasRecap, true
}

// ErrDisabled is returned by SearchWork when the client has no base URL.
var ErrDisabled = errors.New("metadata service disabled")

// SearchResult is one hit from a metadata search (used by the smoke check /
// future needs-core assist).
type SearchResult struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title"`
	Name  string `json:"name"`
}

// Search runs a free-text query against the metadata search endpoint. It returns
// ErrDisabled when unconfigured and a transport error otherwise.
func (c *Client) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	var res struct {
		Results []SearchResult `json:"results"`
	}
	found, ok := c.getJSON(ctx, "/api/v1/search?q="+url.QueryEscape(query), &res)
	if !ok {
		return nil, fmt.Errorf("metadata search failed for %q", query)
	}
	if !found {
		return nil, nil
	}
	return res.Results, nil
}
