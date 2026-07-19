package metaops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// workRow is a fake work's title + sidecar presence.
type workRow struct {
	title                 string
	seriesName, seriesPos string
	c, r                  bool
}

// cardRow is one work-kind search hit the fake returns.
type cardRow struct {
	id, title, author, seriesName, seriesPos string
}

// metaServer is a configurable fake meta.audiosilo.app.
type metaServer struct {
	lookup map[string]string  // asin/isbn -> work id ("" => 404)
	work   map[string]workRow // work id -> detail (absent => 404)
	search []cardRow          // work hits returned for any /search
	extra  string             // an extra non-work result line to prove filtering

	mu       sync.Mutex // guards requests (concurrent coverage workers hit the fake)
	requests map[string]int
}

// count records a request to endpoint (concurrency-safe).
func (s *metaServer) count(endpoint string) {
	s.mu.Lock()
	s.requests[endpoint]++
	s.mu.Unlock()
}

// reqCount returns how many times endpoint has been requested.
func (s *metaServer) reqCount(endpoint string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[endpoint]
}

func (s *metaServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		s.count("lookup")
		key := r.URL.Query().Get("asin")
		if key == "" {
			key = r.URL.Query().Get("isbn")
		}
		id, ok := s.lookup[key]
		if !ok || id == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"work":{"id":"` + id + `"}}`))
	})
	mux.HandleFunc("/api/v1/works/", func(w http.ResponseWriter, r *http.Request) {
		s.count("works")
		id := r.URL.Path[len("/api/v1/works/"):]
		wk, ok := s.work[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body := `{"id":"` + id + `","title":"` + wk.title + `"`
		if wk.seriesName != "" {
			body += `,"series":[{"id":"s","name":"` + wk.seriesName + `","position":"` + wk.seriesPos + `"}]`
		}
		if wk.c {
			body += `,"characters":[{"id":"x","name":"X"}]`
		}
		if wk.r {
			body += `,"recaps":[{"through":{"chapter":1},"text":"t"}]`
		}
		body += `}`
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, _ *http.Request) {
		s.count("search")
		body := `{"results":[`
		parts := make([]string, 0, len(s.search)+1)
		for _, c := range s.search {
			series := "null"
			if c.seriesName != "" {
				series = `{"id":"s","name":"` + c.seriesName + `","position":"` + c.seriesPos + `"}`
			}
			parts = append(parts, `{"kind":"work","id":"`+c.id+`","title":"`+c.title+
				`","authors":[{"id":"p","name":"`+c.author+`"}],"series":`+series+`,"cover_url":"http://x/c.jpg"}`)
		}
		if s.extra != "" {
			parts = append(parts, s.extra)
		}
		for i, p := range parts {
			if i > 0 {
				body += ","
			}
			body += p
		}
		body += `]}`
		_, _ = w.Write([]byte(body))
	})
	return mux
}

func newMeta(t *testing.T, s *metaServer) (*Client, *httptest.Server) {
	t.Helper()
	if s.requests == nil {
		s.requests = map[string]int{}
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return NewClient(srv.URL), srv
}

func TestCoverageByASINAndISBN(t *testing.T) {
	s := &metaServer{
		lookup: map[string]string{"B-ASIN": "w1", "978-ISBN": "w2"},
		work: map[string]workRow{
			"w1": {title: "Work One", seriesName: "Matched Saga", seriesPos: "2", c: true, r: false},
			"w2": {title: "Work Two", c: true, r: true},
		},
	}
	c, _ := newMeta(t, s)
	ctx := context.Background()

	// ASIN match: known, sidecar presence from work detail, matched_by asin, no
	// work_title (an exact identifier needs no confirmation).
	got, _ := c.CoverageFor(ctx, BookIdentity{ASIN: "B-ASIN"})
	if !got.Known || got.MatchedBy != "asin" || !got.HasCharacters || got.HasRecaps || got.WorkTitle != "" {
		t.Fatalf("asin coverage = %+v", got)
	}
	if got.WorkID != "w1" {
		t.Fatalf("asin work id = %q", got.WorkID)
	}
	if got.Series == nil || got.Series.Name != "Matched Saga" || got.Series.Position != "2" {
		t.Fatalf("asin series metadata = %+v", got.Series)
	}

	// ISBN match (no asin present) resolves via the isbn lookup.
	got, _ = c.CoverageFor(ctx, BookIdentity{ISBN: "978-ISBN"})
	if !got.Known || got.MatchedBy != "isbn" || !got.HasCharacters || !got.HasRecaps {
		t.Fatalf("isbn coverage = %+v", got)
	}

	// ASIN takes precedence over ISBN when both resolve.
	got, _ = c.CoverageFor(ctx, BookIdentity{ASIN: "B-ASIN", ISBN: "978-ISBN"})
	if got.MatchedBy != "asin" || got.WorkID != "w1" {
		t.Fatalf("asin-precedence = %+v", got)
	}
	// The second identical call is cache-served (no new lookup requests).
	before := s.reqCount("lookup")
	_, _ = c.CoverageFor(ctx, BookIdentity{ASIN: "B-ASIN"})
	if s.reqCount("lookup") != before {
		t.Errorf("asin lookup not cached: %d -> %d", before, s.reqCount("lookup"))
	}
}

func TestCoverageSearchFallbackAccept(t *testing.T) {
	// No asin/isbn: the fuzzy fallback matches by title + author.
	s := &metaServer{
		work: map[string]workRow{"w-hedge": {title: "The Hedge Wizard", c: false, r: false}},
		search: []cardRow{{
			id: "w-hedge", title: "The Hedge Wizard", author: "Alex Maher",
			seriesName: "Hedge", seriesPos: "1",
		}},
		extra: `{"kind":"person","id":"p1","name":"Alex Maher"}`,
	}
	c, _ := newMeta(t, s)
	ctx := context.Background()

	got, _ := c.CoverageFor(ctx, BookIdentity{Title: "The Hedge Wizard", Authors: []string{"Alex Maher"}})
	if !got.Known || got.MatchedBy != "search" || got.WorkID != "w-hedge" {
		t.Fatalf("search-accept coverage = %+v", got)
	}
	// The search match carries the work title so the user can verify it.
	if got.WorkTitle != "The Hedge Wizard" {
		t.Errorf("search match missing work_title: %+v", got)
	}
	if got.Series == nil || got.Series.Name != "Hedge" || got.Series.Position != "1" {
		t.Errorf("search match missing series metadata: %+v", got.Series)
	}
	// The verdict is cached: a second call issues no new search.
	before := s.reqCount("search")
	_, _ = c.CoverageFor(ctx, BookIdentity{Title: "The Hedge Wizard", Authors: []string{"Alex Maher"}})
	if s.reqCount("search") != before {
		t.Errorf("search verdict not cached: %d -> %d", before, s.reqCount("search"))
	}
}

func TestCoverageSearchFallbackReject(t *testing.T) {
	// The only candidate has a different author, so match.Best rejects it -> unknown.
	s := &metaServer{
		work:   map[string]workRow{"w-hedge": {title: "The Hedge Wizard"}},
		search: []cardRow{{id: "w-hedge", title: "The Hedge Wizard", author: "Someone Else"}},
	}
	c, _ := newMeta(t, s)
	ctx := context.Background()

	got, _ := c.CoverageFor(ctx, BookIdentity{Title: "A Totally Different Book", Authors: []string{"Alex Maher"}})
	if !got.Available || got.Known {
		t.Fatalf("search-reject coverage = %+v (want available/unknown)", got)
	}
}

// TestSearchVerdictKeyIncludesSeries proves the fuzzy-match verdict cache keys on
// the series identity too: two works sharing title + author but sitting in
// different series each resolve to THEIR OWN work - the second must not inherit the
// first's cached verdict.
func TestSearchVerdictKeyIncludesSeries(t *testing.T) {
	s := &metaServer{
		work: map[string]workRow{
			"wA": {title: "Test", c: true},
			"wB": {title: "Test", r: true},
		},
		search: []cardRow{
			{id: "wA", title: "Test", author: "Auth", seriesName: "Alpha", seriesPos: "1"},
			{id: "wB", title: "Test", author: "Auth", seriesName: "Beta", seriesPos: "1"},
		},
	}
	c, _ := newMeta(t, s)
	ctx := context.Background()

	a, _ := c.CoverageFor(ctx, BookIdentity{
		Title: "Test", Authors: []string{"Auth"}, Series: "Alpha", SeriesPos: "1",
	})
	if !a.Known || a.WorkID != "wA" {
		t.Fatalf("series Alpha should resolve to wA: %+v", a)
	}
	b, _ := c.CoverageFor(ctx, BookIdentity{
		Title: "Test", Authors: []string{"Auth"}, Series: "Beta", SeriesPos: "1",
	})
	if !b.Known || b.WorkID != "wB" {
		t.Fatalf("series Beta should resolve to wB (not inherit wA's cached verdict): %+v", b)
	}
}

func TestCoverageForWork(t *testing.T) {
	s := &metaServer{work: map[string]workRow{"w-1": {
		title: "Manual Work", seriesName: "Manual Saga", seriesPos: "4", c: true, r: false,
	}}}
	c, _ := newMeta(t, s)
	ctx := context.Background()

	// A resolvable id -> manual match with work title + sidecar presence.
	got, err := c.CoverageForWork(ctx, "w-1")
	if err != nil {
		t.Fatalf("CoverageForWork: %v", err)
	}
	if !got.Known || got.MatchedBy != "manual" || got.WorkTitle != "Manual Work" || !got.HasCharacters {
		t.Fatalf("manual coverage = %+v", got)
	}
	if got.Series == nil || got.Series.Name != "Manual Saga" || got.Series.Position != "4" {
		t.Fatalf("manual series metadata = %+v", got.Series)
	}

	// A stale id -> ErrWorkNotFound (a clean upstream 404, mappable to a 4xx).
	if _, err := c.CoverageForWork(ctx, "w-missing"); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("missing work = %v, want ErrWorkNotFound", err)
	}

	// A disabled client -> ErrDisabled.
	if _, err := NewClient("").CoverageForWork(ctx, "w-1"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled CoverageForWork = %v, want ErrDisabled", err)
	}

	// A down upstream -> a (non-sentinel) transport error, not ErrWorkNotFound.
	dead := NewClient("http://127.0.0.1:0")
	if _, err := dead.CoverageForWork(ctx, "w-1"); err == nil || errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("unreachable CoverageForWork = %v, want a transport error", err)
	}
}

func TestCoverageNoIdentityDisabledUnreachable(t *testing.T) {
	ctx := context.Background()

	// Enabled but the book has nothing to resolve on: available, unknown.
	s := &metaServer{}
	c, _ := newMeta(t, s)
	none, _ := c.CoverageFor(ctx, BookIdentity{})
	if !none.Available || none.Known {
		t.Fatalf("no identity: %+v", none)
	}

	// Disabled: no base URL -> unavailable.
	off := NewClient("")
	if got, _ := off.CoverageFor(ctx, BookIdentity{ASIN: "B1"}); got.Available {
		t.Fatalf("disabled client should be unavailable: %+v", got)
	}

	// Unreachable upstream degrades to unavailable, never an error.
	dead := NewClient("http://127.0.0.1:0")
	got, err := dead.CoverageFor(ctx, BookIdentity{ASIN: "B1"})
	if err != nil {
		t.Fatalf("unreachable should not error: %v", err)
	}
	if got.Available {
		t.Fatalf("unreachable should be unavailable: %+v", got)
	}
}

func TestSearchWorksProxy(t *testing.T) {
	s := &metaServer{
		search: []cardRow{
			{id: "w1", title: "Hedge Wizard", author: "Alex Maher", seriesName: "Hedge", seriesPos: "1"},
			{id: "w2", title: "Second", author: "Jane Doe"},
		},
		extra: `{"kind":"series","id":"s1","name":"Hedge","works":3}`,
	}
	c, _ := newMeta(t, s)
	ctx := context.Background()

	res, err := c.SearchWorks(ctx, "hedge", 20)
	if err != nil {
		t.Fatalf("SearchWorks: %v", err)
	}
	// Only work hits survive; authors are flattened to names; series carried through.
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2 (non-work filtered): %+v", len(res), res)
	}
	if res[0].ID != "w1" || len(res[0].Authors) != 1 || res[0].Authors[0] != "Alex Maher" {
		t.Fatalf("first result = %+v", res[0])
	}
	if res[0].Series == nil || res[0].Series.Name != "Hedge" || res[0].Series.Position != "1" {
		t.Fatalf("first result series = %+v", res[0].Series)
	}
	if res[1].Series != nil {
		t.Errorf("second result should have no series: %+v", res[1].Series)
	}
	if res[0].CoverURL == "" {
		t.Errorf("cover_url not carried through: %+v", res[0])
	}

	// The proxy is cached briefly (no second upstream call for the same query/limit).
	before := s.reqCount("search")
	if _, err := c.SearchWorks(ctx, "hedge", 20); err != nil {
		t.Fatal(err)
	}
	if s.reqCount("search") != before {
		t.Errorf("proxy not cached: %d -> %d", before, s.reqCount("search"))
	}

	// Disabled -> ErrDisabled.
	if _, err := NewClient("").SearchWorks(ctx, "x", 20); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled SearchWorks = %v, want ErrDisabled", err)
	}
	// Unreachable -> a transport error.
	if _, err := NewClient("http://127.0.0.1:0").SearchWorks(ctx, "x", 20); err == nil {
		t.Fatalf("unreachable SearchWorks should error")
	}
}

// jsonRoundTrip guards the wire tags used by the web UI (matched_by/work_title
// are omitempty; coverage is always present).
func TestCoverageJSONTags(t *testing.T) {
	b, _ := json.Marshal(Coverage{Available: true, Known: true, WorkID: "w", MatchedBy: "search", WorkTitle: "T"})
	for _, key := range []string{`"available"`, `"known"`, `"work_id"`, `"matched_by"`, `"work_title"`, `"has_characters"`, `"has_recaps"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("coverage JSON missing %s: %s", key, b)
		}
	}
	// An unmatched coverage omits the optional fields.
	b, _ = json.Marshal(Coverage{Available: true})
	if strings.Contains(string(b), `"matched_by"`) || strings.Contains(string(b), `"work_title"`) {
		t.Errorf("unmatched coverage should omit matched_by/work_title: %s", b)
	}
}

// TestTTLCacheEvictsAtCap proves the per-cache entry cap holds: putting more
// distinct keys than cacheCap never grows the map past the cap.
func TestTTLCacheEvictsAtCap(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	c := newTTLCache[int, int](func() time.Time { return now }, time.Hour)
	for i := range cacheCap + 100 {
		c.put(i, i)
		if len(c.items) > cacheCap {
			t.Fatalf("cap exceeded after %d puts: len=%d", i+1, len(c.items))
		}
	}
	if len(c.items) != cacheCap {
		t.Fatalf("final len = %d, want %d", len(c.items), cacheCap)
	}
}

// TestTTLCacheEvictsExpiredFirst proves eviction drops expired entries before
// live ones: a fresh entry survives when the map is full of expired entries.
func TestTTLCacheEvictsExpiredFirst(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	clock := func() time.Time { return now }
	c := newTTLCache[int, int](clock, time.Hour)

	// Fill to the cap with entries that will later expire.
	for i := range cacheCap {
		c.put(i, i)
	}
	// Advance past the TTL so every existing entry is expired. This put finds the
	// map at cap and evicts: the whole expired batch goes first, leaving room for
	// the fresh entry.
	now = now.Add(2 * time.Hour)
	c.put(-1, -1)
	c.put(-2, -2)
	if len(c.items) > cacheCap {
		t.Fatalf("cap exceeded: %d", len(c.items))
	}
	if _, ok := c.get(-1); !ok {
		t.Error("fresh entry -1 evicted despite expired entries being available")
	}
	if _, ok := c.get(-2); !ok {
		t.Error("newest entry -2 missing after eviction")
	}
	// A stale entry from the first batch must be gone (expired-first eviction).
	if _, ok := c.get(0); ok {
		t.Error("expired entry 0 survived eviction")
	}
}
