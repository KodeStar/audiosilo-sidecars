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

// workRow is a fake work's title + sidecar presence. chars, when set, lists the
// work's character names for the series-glossary tests; when it is empty and c is
// true the work reports one placeholder character, which is all the coverage tests
// need.
type workRow struct {
	title                 string
	seriesName, seriesPos string
	c, r                  bool
	chars                 []string
}

// cardRow is one work-kind search hit the fake returns.
type cardRow struct {
	id, title, author, seriesName, seriesPos string
	narrators                                []string
}

// metaServer is a configurable fake meta.audiosilo.app.
type metaServer struct {
	lookup map[string]string  // asin/isbn -> work id ("" => 404)
	work   map[string]workRow // work id -> detail (absent => 404)
	search []cardRow          // work hits returned for any /search
	// searchBy, when non-nil, makes /search query-sensitive (an unlisted query
	// returns nothing) - which is what the retrieval ladder is about.
	searchBy    map[string][]cardRow
	extra       string              // an extra non-work result line to prove filtering
	seriesWorks map[string][]string // series id -> member work ids (absent => 404)
	onWorks     func(id string)     // optional hook, called on each /works/{id} request

	mu       sync.Mutex // guards requests (concurrent coverage workers hit the fake)
	requests map[string]int
	queries  []string // every /search q, in the order received
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

// seenQueries returns the /search queries received, in order.
func (s *metaServer) seenQueries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
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
		// onWorks lets a test interrupt a fan-out deterministically (no sleeps) by
		// cancelling the caller's context part-way through.
		if s.onWorks != nil {
			s.onWorks(id)
		}
		wk, ok := s.work[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body := `{"id":"` + id + `","title":"` + wk.title + `"`
		if wk.seriesName != "" {
			body += `,"series":[{"id":"s","name":"` + wk.seriesName + `","position":"` + wk.seriesPos + `"}]`
		}
		switch {
		case len(wk.chars) > 0:
			parts := make([]string, 0, len(wk.chars))
			for i, n := range wk.chars {
				parts = append(parts, `{"id":"c`+string(rune('a'+i))+`","name":"`+n+`"}`)
			}
			body += `,"characters":[` + strings.Join(parts, ",") + `]`
		case wk.c:
			body += `,"characters":[{"id":"x","name":"X"}]`
		}
		if wk.r {
			body += `,"recaps":[{"through":{"chapter":1},"text":"t"}]`
		}
		body += `}`
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v1/series/", func(w http.ResponseWriter, r *http.Request) {
		s.count("series")
		id := r.URL.Path[len("/api/v1/series/"):]
		ids, ok := s.seriesWorks[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		parts := make([]string, 0, len(ids))
		for _, wid := range ids {
			parts = append(parts, `{"position":"1","work":{"id":"`+wid+`"}}`)
		}
		_, _ = w.Write([]byte(`{"id":"` + id + `","name":"S","works":[` + strings.Join(parts, ",") + `]}`))
	})
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		s.count("search")
		q := r.URL.Query().Get("q")
		s.mu.Lock()
		s.queries = append(s.queries, q)
		s.mu.Unlock()
		cards := s.search
		if s.searchBy != nil {
			cards = s.searchBy[q]
		}
		body := `{"results":[`
		parts := make([]string, 0, len(cards)+1)
		for _, c := range cards {
			series := "null"
			if c.seriesName != "" {
				series = `{"id":"s","name":"` + c.seriesName + `","position":"` + c.seriesPos + `"}`
			}
			narrators := ""
			for i, n := range c.narrators {
				if i > 0 {
					narrators += ","
				}
				narrators += `{"id":"n` + string(rune('a'+i)) + `","name":"` + n + `"}`
			}
			parts = append(parts, `{"kind":"work","id":"`+c.id+`","title":"`+c.title+
				`","authors":[{"id":"p","name":"`+c.author+`"}],"narrators":[`+narrators+
				`],"series":`+series+`,"cover_url":"http://x/c.jpg"}`)
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

// wantQueries asserts the exact ordered list of /search queries the fake saw.
// The order IS the contract: the raw title must go first (a book that already
// resolved costs one request) and nothing may be sent after the accepting rung.
func wantQueries(t *testing.T, s *metaServer, want ...string) {
	t.Helper()
	got := s.seenQueries()
	if len(got) != len(want) {
		t.Fatalf("queries = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("query %d = %q, want %q (all: %q)", i, got[i], want[i], got)
		}
	}
}

// TestSearchLadderPreSubtitleRescue: a decorated shelf title retrieves nothing,
// while its pre-subtitle segment is an exact hit. It also pins the ladder's
// order and its early exit - the raw title goes first, and the rungs past the
// accepting one are never sent.
func TestSearchLadderPreSubtitleRescue(t *testing.T) {
	s := &metaServer{
		work: map[string]workRow{"w-sm": {title: "Supermage", c: true}},
		searchBy: map[string][]cardRow{
			"Supermage": {{id: "w-sm", title: "Supermage", author: "Michael Head"}},
		},
	}
	c, _ := newMeta(t, s)

	got, _ := c.CoverageFor(context.Background(), BookIdentity{
		Title:   "Supermage : Rise To Omniscience, Book 1",
		Authors: []string{"Michael Head"},
	})
	if !got.Known || got.MatchedBy != "search" || got.WorkID != "w-sm" {
		t.Fatalf("pre-subtitle rescue = %+v", got)
	}
	wantQueries(t, s,
		"Supermage : Rise To Omniscience, Book 1", // 1. raw title
		"Supermage Rise To Omniscience Book 1",    // 2. punctuation-normalized
		"Supermage : Rise To Omniscience",         // 3. CleanTitle
		"Supermage",                               // 4. pre-subtitle - accepted, ladder stops
	)
}

// TestSearchLadderPunctuationRescue: "(Unabridged)" + a colon hide a work that
// the bare words retrieve exactly.
func TestSearchLadderPunctuationRescue(t *testing.T) {
	s := &metaServer{
		work: map[string]workRow{"w-halo": {title: "Halo: Primordium", r: true}},
		searchBy: map[string][]cardRow{
			"Halo Primordium": {{id: "w-halo", title: "Halo: Primordium", author: "Greg Bear"}},
		},
	}
	c, _ := newMeta(t, s)

	got, _ := c.CoverageFor(context.Background(), BookIdentity{
		Title: "Halo: Primordium (Unabridged)", Authors: []string{"Greg Bear"},
	})
	if !got.Known || got.WorkID != "w-halo" || !got.HasRecaps {
		t.Fatalf("punctuation rescue = %+v", got)
	}
	wantQueries(t, s, "Halo: Primordium (Unabridged)", "Halo Primordium")
}

// TestSearchLadderPostSeparatorRescue: a shelf prefix ("Artemis Fowl 4 - ")
// buries the title; the tail after the last separator is the work.
func TestSearchLadderPostSeparatorRescue(t *testing.T) {
	s := &metaServer{
		work: map[string]workRow{"w-opal": {title: "The Opal Deception"}},
		searchBy: map[string][]cardRow{
			"The Opal Deception": {{id: "w-opal", title: "The Opal Deception", author: "Eoin Colfer"}},
		},
	}
	c, _ := newMeta(t, s)

	got, _ := c.CoverageFor(context.Background(), BookIdentity{
		Title: "Artemis Fowl 4 - The Opal Deception", Authors: []string{"Eoin Colfer"},
	})
	if !got.Known || got.WorkID != "w-opal" {
		t.Fatalf("post-separator rescue = %+v", got)
	}
	wantQueries(t, s, "Artemis Fowl 4 - The Opal Deception", "The Opal Deception")
}

// TestSearchLadderPathHints: the tags carry a shortcode title, but the folder
// leaf carries the real one. The card is scored against the LEAF, not the
// shortcode - scoring it against "RO07" would reject the card the rung found.
func TestSearchLadderPathHints(t *testing.T) {
	s := &metaServer{
		work: map[string]workRow{"w-sq": {title: "Sandqueen", c: true, r: true}},
		searchBy: map[string][]cardRow{
			"Sandqueen": {{id: "w-sq", title: "Sandqueen", author: "Michael Head"}},
		},
	}
	c, _ := newMeta(t, s)

	got, _ := c.CoverageFor(context.Background(), BookIdentity{
		Title: "RO07", Authors: []string{"Michael Head"},
		FolderName: "RO07 - Sandqueen", ParentDir: "Rise to Omniscience",
	})
	if !got.Known || got.MatchedBy != "search" || got.WorkID != "w-sq" {
		t.Fatalf("path-hint rescue = %+v", got)
	}
	// The parent-folder rung sits AFTER the leaf rung, so it is never reached.
	wantQueries(t, s, "RO07", "RO07 Michael Head", "Sandqueen")
}

// TestSearchNarratorGate covers the author gate's narrator evidence, in both
// directions, plus the control that keeps it from matching anything.
func TestSearchNarratorGate(t *testing.T) {
	// (a) The shelf tags the NARRATOR as the author; the card carries him as a
	// narrator and the real author alongside.
	t.Run("query author is a card narrator", func(t *testing.T) {
		s := &metaServer{
			work: map[string]workRow{"w-dragon": {title: "How to Break a Dragon's Heart", c: true}},
			search: []cardRow{{
				id: "w-dragon", title: "How to Break a Dragon's Heart",
				author: "Cressida Cowell", narrators: []string{"David Tennant"},
			}},
		}
		c, _ := newMeta(t, s)
		got, _ := c.CoverageFor(context.Background(), BookIdentity{
			Title: "How to Break a Dragon's Heart", Authors: []string{"David Tennant"},
		})
		if !got.Known || got.MatchedBy != "search" || got.WorkID != "w-dragon" {
			t.Fatalf("narrator-as-author coverage = %+v", got)
		}
	})

	// (b) The reverse: the local narrator credit names the card's AUTHOR (an
	// author narrating his own book, behind a junk author tag).
	t.Run("local narrator is a card author", func(t *testing.T) {
		s := &metaServer{
			work: map[string]workRow{"w-ll": {title: "Legends and Lattes", r: true}},
			search: []cardRow{{
				id: "w-ll", title: "Legends and Lattes", author: "Travis Baldree",
			}},
		}
		c, _ := newMeta(t, s)
		got, _ := c.CoverageFor(context.Background(), BookIdentity{
			Title: "Legends and Lattes", Authors: []string{"Audible Studios"},
			Narrators: []string{"Travis Baldree"},
		})
		if !got.Known || got.WorkID != "w-ll" {
			t.Fatalf("narrator-evidence coverage = %+v", got)
		}
	})

	// Control: an exact-title card with no author OR narrator link stays a miss -
	// the gate widens the evidence, it does not remove it.
	t.Run("no link is still a miss", func(t *testing.T) {
		s := &metaServer{
			work: map[string]workRow{"w-dragon": {title: "How to Break a Dragon's Heart"}},
			search: []cardRow{{
				id: "w-dragon", title: "How to Break a Dragon's Heart",
				author: "Cressida Cowell", narrators: []string{"Other Person"},
			}},
		}
		c, _ := newMeta(t, s)
		got, _ := c.CoverageFor(context.Background(), BookIdentity{
			Title: "How to Break a Dragon's Heart", Authors: []string{"David Tennant"},
		})
		if !got.Available || got.Known {
			t.Fatalf("unlinked card = %+v (want available/unknown)", got)
		}
	})
}

// TestSearchLadderExhaustsAndCaches pins the full ladder for a book nothing
// matches (its order, its de-duplication of rungs that collapse onto each
// other, and its bounded length) and proves ONE negative verdict covers the
// whole ladder: the second call issues no request at all.
func TestSearchLadderExhaustsAndCaches(t *testing.T) {
	s := &metaServer{searchBy: map[string][]cardRow{}}
	c, _ := newMeta(t, s)
	id := BookIdentity{
		Title:   "Supermage : Rise To Omniscience, Book 1",
		Authors: []string{"Michael Head"},
		// Real path hints, so the last two rungs are exercised.
		FolderName: "RO07 - Sandqueen", ParentDir: "Rise to Omniscience",
	}

	got, _ := c.CoverageFor(context.Background(), id)
	if !got.Available || got.Known {
		t.Fatalf("exhausted ladder = %+v (want available/unknown)", got)
	}
	wantQueries(t, s,
		"Supermage : Rise To Omniscience, Book 1",      // 1. raw title
		"Supermage Rise To Omniscience Book 1",         // 2. punctuation-normalized
		"Supermage : Rise To Omniscience",              // 3. CleanTitle (rung 6 collapses onto it)
		"Supermage",                                    // 4. pre-subtitle
		"Rise To Omniscience, Book 1",                  // 5. post-separator tail
		"Supermage : Rise To Omniscience Michael Head", // 7. clean title + author
		"Sandqueen",           // 8. folder leaf tail
		"Rise to Omniscience", // 9. parent folder
	)

	before := s.reqCount("search")
	got, _ = c.CoverageFor(context.Background(), id)
	if got.Known {
		t.Fatalf("cached verdict = %+v", got)
	}
	if s.reqCount("search") != before {
		t.Errorf("negative ladder verdict not cached: %d -> %d", before, s.reqCount("search"))
	}
}

// TestSearchLadderShortTitle: a genuinely short real title ("It") must still be
// asked upstream. Every derived rung is below minQueryLen, so a floor applied to
// rung 1 too would leave an EMPTY ladder and cache a negative verdict having
// issued no request at all.
func TestSearchLadderShortTitle(t *testing.T) {
	s := &metaServer{
		work:     map[string]workRow{"w-it": {title: "It", c: true}},
		searchBy: map[string][]cardRow{"It": {{id: "w-it", title: "It", author: "Stephen King"}}},
	}
	c, _ := newMeta(t, s)

	got, _ := c.CoverageFor(context.Background(), BookIdentity{
		Title: "It", Authors: []string{"Stephen King"},
	})
	if !got.Known || got.MatchedBy != "search" || got.WorkID != "w-it" {
		t.Fatalf("short-title match = %+v", got)
	}
	// Exactly one query, and it is the raw title.
	wantQueries(t, s, "It")
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
