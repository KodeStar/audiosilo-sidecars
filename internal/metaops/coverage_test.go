package metaops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
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

// anySearch is the searchBy key answering EVERY query the map does not list by
// name - the "this test does not care which rung retrieved the card" default.
const anySearch = "*"

// metaServer is a configurable fake meta.audiosilo.app.
type metaServer struct {
	lookup map[string]string  // asin/isbn -> work id ("" => 404)
	work   map[string]workRow // work id -> detail (absent => 404)
	// searchBy maps a /search query to the work hits it returns, which is what the
	// retrieval ladder is about: a query listed by name answers only itself, an
	// unlisted one returns nothing, and anySearch answers whatever is left.
	searchBy    map[string][]cardRow
	extra       string              // an extra non-work result line to prove filtering
	seriesWorks map[string][]string // series id -> member work ids (absent => 404)
	onWorks     func(id string)     // optional hook, called on each /works/{id} request

	mu       sync.Mutex // guards requests (concurrent coverage workers hit the fake)
	requests map[string]int
	queries  []string // every /search q, in the order received (and its own count)
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

// searchCount is how many /search requests arrived - the query log's length, not
// a second counter that could disagree with it.
func (s *metaServer) searchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queries)
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
		q := r.URL.Query().Get("q")
		s.mu.Lock()
		s.queries = append(s.queries, q)
		s.mu.Unlock()
		cards, ok := s.searchBy[q]
		if !ok {
			cards = s.searchBy[anySearch]
		}
		body := `{"results":[`
		parts := make([]string, 0, len(cards)+1)
		for _, c := range cards {
			series := "null"
			if c.seriesName != "" {
				series = `{"id":"s","name":"` + c.seriesName + `","position":"` + c.seriesPos + `"}`
			}
			narrators := make([]string, 0, len(c.narrators))
			for _, n := range c.narrators {
				narrators = append(narrators, `{"name":"`+n+`"}`)
			}
			parts = append(parts, `{"kind":"work","id":"`+c.id+`","title":"`+c.title+
				`","authors":[{"id":"p","name":"`+c.author+`"}],"narrators":[`+
				strings.Join(narrators, ",")+`],"series":`+series+`,"cover_url":"http://x/c.jpg"}`)
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
		searchBy: map[string][]cardRow{anySearch: {{
			id: "w-hedge", title: "The Hedge Wizard", author: "Alex Maher",
			seriesName: "Hedge", seriesPos: "1",
		}}},
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
	before := s.searchCount()
	_, _ = c.CoverageFor(ctx, BookIdentity{Title: "The Hedge Wizard", Authors: []string{"Alex Maher"}})
	if s.searchCount() != before {
		t.Errorf("search verdict not cached: %d -> %d", before, s.searchCount())
	}
}

func TestCoverageSearchFallbackReject(t *testing.T) {
	// The only candidate has a different author, so match.Best rejects it -> unknown.
	s := &metaServer{
		work: map[string]workRow{"w-hedge": {title: "The Hedge Wizard"}},
		searchBy: map[string][]cardRow{
			anySearch: {{id: "w-hedge", title: "The Hedge Wizard", author: "Someone Else"}},
		},
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
		searchBy: map[string][]cardRow{anySearch: {
			{id: "wA", title: "Test", author: "Auth", seriesName: "Alpha", seriesPos: "1"},
			{id: "wB", title: "Test", author: "Auth", seriesName: "Beta", seriesPos: "1"},
		}},
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
	if got := s.seenQueries(); !slices.Equal(got, want) {
		t.Fatalf("queries = %q, want %q", got, want)
	}
}

// TestSearchLadderRescues walks the retrieval ladder's rungs: in each case the
// book's own title retrieves NOTHING and a later rung is an exact hit. The
// expected query list pins both halves of the contract - the raw title is always
// rung 1, and the ladder stops at the rung that matched.
func TestSearchLadderRescues(t *testing.T) {
	for _, tc := range []struct {
		name        string
		title       string
		author      string
		folder      string
		parent      string
		hitQuery    string // the ONLY query the fake answers
		cardTitle   string
		cardSeries  string
		cardPos     string
		wantQueries []string
	}{
		{
			name:   "pre-subtitle segment",
			title:  "Supermage : Rise To Omniscience, Book 1",
			author: "Michael Head",
			// The series/volume decoration hides a work catalogued under one word.
			// The card states the position the title claims (Book 1), which the
			// volume veto requires of any rung that dropped the number: the rescue
			// query "Supermage" no longer carries it, so a position-less card could
			// as easily be volume 6.
			hitQuery: "Supermage", cardTitle: "Supermage",
			cardSeries: "Rise To Omniscience", cardPos: "1",
			wantQueries: []string{
				"Supermage : Rise To Omniscience, Book 1", // 1. raw title
				"Supermage Rise To Omniscience Book 1",    // 2. punctuation-normalized
				"Supermage : Rise To Omniscience",         // 3. CleanTitle
				"Supermage",                               // 4. pre-subtitle - accepted
			},
		},
		{
			name:   "punctuation and edition noise",
			title:  "Halo: Primordium (Unabridged)",
			author: "Greg Bear",
			// The tokenizer never splits "Halo:", so the bare words are the rescue.
			hitQuery: "Halo Primordium", cardTitle: "Halo: Primordium",
			wantQueries: []string{"Halo: Primordium (Unabridged)", "Halo Primordium"},
		},
		{
			name:   "post-separator tail",
			title:  "Artemis Fowl 4 - The Opal Deception",
			author: "Eoin Colfer",
			// A shelf prefix buries the work's own title behind the series and volume.
			hitQuery: "The Opal Deception", cardTitle: "The Opal Deception",
			wantQueries: []string{"Artemis Fowl 4 - The Opal Deception", "The Opal Deception"},
		},
		{
			name:   "path hints",
			title:  "RO07",
			author: "Michael Head",
			folder: "RO07 - Sandqueen", parent: "Rise to Omniscience",
			// The tags carry a shortcode; the folder leaf carries the real title, and
			// the card is scored against the LEAF (scoring it against "RO07" would
			// reject the very card the rung retrieved).
			hitQuery: "Sandqueen", cardTitle: "Sandqueen",
			// The parent-folder rung sits after the leaf rung, so it is never reached.
			wantQueries: []string{"RO07", "RO07 Michael Head", "Sandqueen"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &metaServer{
				work: map[string]workRow{"w-hit": {title: tc.cardTitle, c: true}},
				searchBy: map[string][]cardRow{
					tc.hitQuery: {{
						id: "w-hit", title: tc.cardTitle, author: tc.author,
						seriesName: tc.cardSeries, seriesPos: tc.cardPos,
					}},
				},
			}
			c, _ := newMeta(t, s)

			got, _ := c.CoverageFor(context.Background(), BookIdentity{
				Title: tc.title, Authors: []string{tc.author},
				FolderName: tc.folder, ParentDir: tc.parent,
			})
			if !got.Known || got.MatchedBy != "search" || got.WorkID != "w-hit" {
				t.Fatalf("coverage = %+v", got)
			}
			wantQueries(t, s, tc.wantQueries...)
		})
	}
}

// TestSearchLadderRawTitleAlwaysSent pins the one rung that is never skipped.
// Before the ladder existed the raw title was always searched; the minimum query
// length is a floor on the DERIVED rungs only, or a book called "It" would search
// nothing at all and still cache a verdict saying it is unknown.
func TestSearchLadderRawTitleAlwaysSent(t *testing.T) {
	t.Run("a two-letter title still resolves", func(t *testing.T) {
		s := &metaServer{
			work:     map[string]workRow{"w-it": {title: "It", c: true}},
			searchBy: map[string][]cardRow{"It": {{id: "w-it", title: "It", author: "Stephen King"}}},
		}
		c, _ := newMeta(t, s)
		got, _ := c.CoverageFor(context.Background(), BookIdentity{
			Title: "It", Authors: []string{"Stephen King"},
		})
		if !got.Known || got.WorkID != "w-it" {
			t.Fatalf("coverage = %+v", got)
		}
		wantQueries(t, s, "It")
	})

	t.Run("and is still sent when nothing matches", func(t *testing.T) {
		s := &metaServer{searchBy: map[string][]cardRow{}}
		c, _ := newMeta(t, s)
		got, _ := c.CoverageFor(context.Background(), BookIdentity{
			Title: "Us", Authors: []string{"Jordan Peele"},
		})
		if !got.Available || got.Known {
			t.Fatalf("coverage = %+v", got)
		}
		wantQueries(t, s, "Us", "Us Jordan Peele")
	})

	t.Run("the floor on derived rungs counts runes, not bytes", func(t *testing.T) {
		// A one-rune CJK folder name is three BYTES: counted as bytes it clears the
		// floor and sends a query of one character.
		s := &metaServer{searchBy: map[string][]cardRow{}}
		c, _ := newMeta(t, s)
		_, _ = c.CoverageFor(context.Background(), BookIdentity{
			Title: "Novel Title", Authors: []string{"Ann Lee"}, ParentDir: "\u672c",
		})
		wantQueries(t, s, "Novel Title", "Novel Title Ann Lee")
	})
}

// TestSearchLadderFolderLeaf covers the folder-leaf rung: what it queries, that a
// same-query rung scored against a different title cannot delete it, and the
// guard that decides whether the leaf may be scored against at all.
func TestSearchLadderFolderLeaf(t *testing.T) {
	// The card's subtitle is REAL words, not genre fluff: CleanTitle drops a fluff
	// subtitle, and the card would then match the tag title too, proving nothing.
	const leafCard = "Sandqueen: The Second Ascent"

	t.Run("the leaf-scored rung survives the same query scored differently", func(t *testing.T) {
		// Tag title and folder agree, so rung 5 (post-separator tail of the TITLE)
		// sends "Sandqueen" scored against the whole tag title - which does not
		// match - before rung 8 sends it scored against the leaf, which does.
		s := &metaServer{
			work:     map[string]workRow{"w-sq": {title: leafCard}},
			searchBy: map[string][]cardRow{"Sandqueen": {{id: "w-sq", title: leafCard, author: "Michael Head"}}},
		}
		c, _ := newMeta(t, s)
		got, _ := c.CoverageFor(context.Background(), BookIdentity{
			Title: "RO07 - Sandqueen", Authors: []string{"Michael Head"},
			FolderName: "RO07 - Sandqueen",
		})
		if !got.Known || got.WorkID != "w-sq" {
			t.Fatalf("coverage = %+v", got)
		}
	})

	t.Run("a leaf with no separator is queried whole", func(t *testing.T) {
		s := &metaServer{
			work:     map[string]workRow{"w-sq": {title: "Sandqueen"}},
			searchBy: map[string][]cardRow{"Sandqueen": {{id: "w-sq", title: "Sandqueen", author: "Michael Head"}}},
		}
		c, _ := newMeta(t, s)
		got, _ := c.CoverageFor(context.Background(), BookIdentity{
			Title: "RO07", Authors: []string{"Michael Head"},
			FolderName: "Sandqueen", ParentDir: "Rise to Omniscience",
		})
		if !got.Known || got.WorkID != "w-sq" {
			t.Fatalf("coverage = %+v", got)
		}
		wantQueries(t, s, "RO07", "RO07 Michael Head", "Sandqueen")
	})

	t.Run("a contradicting tag title keeps its own scoring", func(t *testing.T) {
		// A mis-shelved folder: its leaf names a DIFFERENT book by the same author.
		// The rung still WIDENS retrieval, but the card is scored against the tag
		// title, so it cannot mint a confident match - a search verdict becomes
		// books.work_id, which the contributing stage attaches sidecars to.
		s := &metaServer{
			work:     map[string]workRow{"w-sq": {title: "Sandqueen"}},
			searchBy: map[string][]cardRow{"Sandqueen": {{id: "w-sq", title: "Sandqueen", author: "Michael Head"}}},
		}
		c, _ := newMeta(t, s)
		got, _ := c.CoverageFor(context.Background(), BookIdentity{
			Title: "The Opal Deception", Authors: []string{"Michael Head"},
			FolderName: "Sandqueen",
		})
		if !got.Available || got.Known {
			t.Fatalf("mis-shelved folder minted a match: %+v", got)
		}
		if !slices.Contains(s.seenQueries(), "Sandqueen") {
			t.Errorf("the leaf rung should still widen RETRIEVAL: %q", s.seenQueries())
		}
	})
}

// TestSearchLadderVolumeVeto covers the wrong-volume guard. A wider query
// retrieves the right SERIES and the matcher cannot tell the volumes apart -
// titleTokens drops pure numbers, so a book-1 card scores a perfect title match
// against book 7 - and the accepted work id flows into books.work_id.
func TestSearchLadderVolumeVeto(t *testing.T) {
	for _, tc := range []struct {
		name       string
		title      string
		author     string
		hitQuery   string
		cardTitle  string
		cardSeries string
		cardPos    string
		wantKnown  bool
	}{
		{
			// The rung-6 shape: "The Wandering Inn - 7" queries "The Wandering Inn",
			// and volume 1's card is a perfect token match for it.
			name:  "a position-less card on a number-dropping rung is refused",
			title: "The Wandering Inn - 7", author: "pirateaba",
			hitQuery: "The Wandering Inn", cardTitle: "The Wandering Inn",
		},
		{
			name:  "the same card stating the claimed volume is accepted",
			title: "The Wandering Inn - 7", author: "pirateaba",
			hitQuery: "The Wandering Inn", cardTitle: "The Wandering Inn",
			cardSeries: "The Wandering Inn", cardPos: "7", wantKnown: true,
		},
		{
			// The pre-subtitle shape: the rescue query drops "Book 2" and lands on
			// the volume-1 card, which says so.
			name:  "an explicitly disagreeing card position is refused",
			title: "Supermage : Rise To Omniscience, Book 2", author: "Michael Head",
			hitQuery: "Supermage", cardTitle: "Supermage",
			cardSeries: "Rise To Omniscience", cardPos: "1",
		},
		{
			// Rung 6 in its own right, and the upstream matcher's series linking:
			// the bare "series + number" title matches the numbered card of that
			// series even though their titles share nothing.
			name:  "a stripped trailing volume still finds the numbered card",
			title: "Vorkosigan Saga 01", author: "Lois McMaster Bujold",
			hitQuery: "Vorkosigan Saga", cardTitle: "Shards of Honour",
			cardSeries: "Vorkosigan Saga", cardPos: "1", wantKnown: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &metaServer{
				work: map[string]workRow{"w-hit": {title: tc.cardTitle}},
				searchBy: map[string][]cardRow{tc.hitQuery: {{
					id: "w-hit", title: tc.cardTitle, author: tc.author,
					seriesName: tc.cardSeries, seriesPos: tc.cardPos,
				}}},
			}
			c, _ := newMeta(t, s)
			got, _ := c.CoverageFor(context.Background(), BookIdentity{
				Title: tc.title, Authors: []string{tc.author},
			})
			if got.Known != tc.wantKnown {
				t.Fatalf("known = %t, want %t (%+v)", got.Known, tc.wantKnown, got)
			}
			if tc.wantKnown && got.WorkID != "w-hit" {
				t.Fatalf("work id = %q", got.WorkID)
			}
		})
	}

	// The control for the number-stripping rung: a title that merely ENDS in a
	// number is matched by the raw-title rung first, so the stripped rung is never
	// even sent - "451" is a temperature, not a volume.
	t.Run("a title that ends in a number is matched raw", func(t *testing.T) {
		s := &metaServer{
			work: map[string]workRow{"w-f451": {title: "Fahrenheit 451"}},
			searchBy: map[string][]cardRow{
				"Fahrenheit 451": {{id: "w-f451", title: "Fahrenheit 451", author: "Ray Bradbury"}},
			},
		}
		c, _ := newMeta(t, s)
		got, _ := c.CoverageFor(context.Background(), BookIdentity{
			Title: "Fahrenheit 451", Authors: []string{"Ray Bradbury"},
		})
		if !got.Known || got.WorkID != "w-f451" {
			t.Fatalf("coverage = %+v", got)
		}
		wantQueries(t, s, "Fahrenheit 451")
	})
}

// TestSearchNarratorGate covers the author gate's narrator evidence, in both
// directions, plus the control that keeps it from matching anything.
func TestSearchNarratorGate(t *testing.T) {
	// (a) The shelf tags the NARRATOR as the author; the card carries him as a
	// narrator and the real author alongside.
	t.Run("query author is a card narrator", func(t *testing.T) {
		s := &metaServer{
			work: map[string]workRow{"w-dragon": {title: "How to Break a Dragon's Heart", c: true}},
			searchBy: map[string][]cardRow{anySearch: {{
				id: "w-dragon", title: "How to Break a Dragon's Heart",
				author: "Cressida Cowell", narrators: []string{"David Tennant"},
			}}},
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
			searchBy: map[string][]cardRow{anySearch: {{
				id: "w-ll", title: "Legends and Lattes", author: "Travis Baldree",
			}}},
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
			searchBy: map[string][]cardRow{anySearch: {{
				id: "w-dragon", title: "How to Break a Dragon's Heart",
				author: "Cressida Cowell", narrators: []string{"Other Person"},
			}}},
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

	before := s.searchCount()
	got, _ = c.CoverageFor(context.Background(), id)
	if got.Known {
		t.Fatalf("cached verdict = %+v", got)
	}
	if s.searchCount() != before {
		t.Errorf("negative ladder verdict not cached: %d -> %d", before, s.searchCount())
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
		searchBy: map[string][]cardRow{anySearch: {
			{id: "w1", title: "Hedge Wizard", author: "Alex Maher", seriesName: "Hedge", seriesPos: "1"},
			{id: "w2", title: "Second", author: "Jane Doe"},
		}},
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
	before := s.searchCount()
	if _, err := c.SearchWorks(ctx, "hedge", 20); err != nil {
		t.Fatal(err)
	}
	if s.searchCount() != before {
		t.Errorf("proxy not cached: %d -> %d", before, s.searchCount())
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

// TestCoverageApplyContributed pins the read-time patch: it only ever ADDS a
// dimension, only to a Known verdict, only when the contributions were made under
// the same work, and it leaves the rest of the verdict alone.
func TestCoverageApplyContributed(t *testing.T) {
	known := Coverage{Available: true, Known: true, WorkID: "w1", MatchedBy: "search"}

	// A landed contribution under this work repairs the stale "needed" verdict.
	got := known
	got.ApplyContributed("w1", true, false)
	if !got.HasCharacters || got.HasRecaps {
		t.Fatalf("patched verdict = %+v", got)
	}
	if got.WorkID != "w1" || got.MatchedBy != "search" || !got.Available {
		t.Fatalf("patch touched the rest of the verdict: %+v", got)
	}

	// An unattached book (no work resolved yet) can only have contributed to the
	// work the scan matched, so the patch applies.
	got = known
	got.ApplyContributed("", false, true)
	if !got.HasRecaps {
		t.Fatalf("unattached book not patched: %+v", got)
	}

	// A book attached to a DIFFERENT work (the core add-work flow's real slug, or
	// a human's later manual match) must not stamp this work's badges.
	got = known
	got.ApplyContributed("w2", true, true)
	if got.HasCharacters || got.HasRecaps {
		t.Fatalf("another work's contributions were applied: %+v", got)
	}

	// Nothing landed: upstream's own coverage survives untouched (additive only).
	got = known
	got.HasRecaps = true
	got.ApplyContributed("w1", false, false)
	if !got.HasRecaps {
		t.Fatalf("empty patch cleared upstream coverage: %+v", got)
	}

	// An unresolved book is never claimed: the badges must stay honestly unknown.
	unknown := Coverage{Available: true}
	unknown.ApplyContributed("", true, true)
	if unknown.HasCharacters || unknown.HasRecaps {
		t.Fatalf("unknown verdict patched: %+v", unknown)
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
