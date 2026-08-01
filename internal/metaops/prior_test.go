package metaops

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// jackReacher builds a fake series shaped like the live incident this feature exists
// for: book 2 is being processed locally, and book 1 was never processed here because
// it is already covered upstream - so the LOCAL store holds no predecessor and the
// book looked like its own series opener.
func jackReacher() *metaServer {
	return &metaServer{
		seriesName: "Jack Reacher",
		work: map[string]workRow{
			"die-trying": {title: "Die Trying", seriesName: "Jack Reacher", seriesPos: "2"},
			"killing-floor": {
				title: "Killing Floor", seriesName: "Jack Reacher", seriesPos: "1",
				authors: []string{"Lee Child"},
				inShort: "Reacher is arrested in Margrave and finds the dead man is his brother.",
				ending:  "Reacher leaves Margrave with the conspiracy broken.",
				recaps: []recapRow{
					{chapter: 6, text: "Reacher is arrested less than an hour after walking into town."},
					{chapter: 30, text: "The counterfeiting operation collapses and Reacher moves on."},
					{chapter: 18, text: "Finlay and Roscoe start to believe him."},
				},
			},
		},
		seriesWorks: map[string][]string{
			"s": {"killing-floor", "die-trying"}, "jack-reacher": {"killing-floor", "die-trying"},
		},
	}
}

// liveSeriesPayload is a VERBATIM (trimmed to two members) capture of
// GET https://meta.audiosilo.app/api/v1/series/jack-reacher. Nothing about it is
// normalized: it exists so a struct-vs-wire drift is caught by a real payload rather
// than by a fake that happens to agree with the struct.
//
// The decisive detail is `work.series` being an OBJECT here. It was once decoded as an
// array, which made json.Unmarshal fail for every real series - reported upward as a
// transport failure, so the prior lookup never worked and SeriesGlossary silently went
// empty. Every fake passed, because none of them emitted the key at all.
const liveSeriesPayload = `{
 "id": "jack-reacher",
 "name": "Jack Reacher",
 "authors": [],
 "works": [
  {
   "position": "1",
   "work": {
    "id": "killing-floor",
    "title": "Killing Floor",
    "authors": [{"id": "lee-child", "name": "Lee Child"}],
    "series": {"id": "jack-reacher", "name": "Jack Reacher", "position": "1"},
    "cover_url": "https://m.media-amazon.com/images/I/41LG8nia4rL.jpg",
    "added_at": "2026-07-12T23:30:06+01:00"
   }
  },
  {
   "position": "2",
   "work": {
    "id": "die-trying",
    "title": "Die Trying",
    "authors": [{"id": "lee-child", "name": "Lee Child"}],
    "series": {"id": "jack-reacher", "name": "Jack Reacher", "position": "2"},
    "cover_url": "https://m.media-amazon.com/images/I/51vI4yx97hL.jpg",
    "added_at": "2026-07-30T14:42:56+01:00"
   }
  }
 ]
}`

func TestSeriesListingDecodesTheLivePayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(liveSeriesPayload))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL)

	feed, found, ok := c.seriesListing(context.Background(), "jack-reacher")
	if !ok {
		t.Fatal("the live payload was reported as a TRANSPORT FAILURE: the decode struct " +
			"does not match the wire (this is what a json.Unmarshal error looks like from here)")
	}
	if !found {
		t.Fatal("found = false for a 200 payload")
	}
	if feed.name != "Jack Reacher" {
		t.Errorf("series name = %q", feed.name)
	}
	want := []seriesEntry{{ID: "killing-floor", Pos: "1"}, {ID: "die-trying", Pos: "2"}}
	if !slices.Equal(feed.entries, want) {
		t.Errorf("entries = %+v, want %+v", feed.entries, want)
	}

	// And the glossary path, which reads through the same decode, must see the members.
	ids, ok := c.seriesWorks(context.Background(), "jack-reacher")
	if !ok || !slices.Equal(ids, []string{"killing-floor", "die-trying"}) {
		t.Errorf("seriesWorks = %v ok=%v; a decode drift silently empties SeriesGlossary", ids, ok)
	}
}

// The position also survives when only the NESTED work states it (the entry-level
// key is the primary source; this is the documented fallback).
func TestSeriesListingFallsBackToTheNestedPosition(t *testing.T) {
	body := `{"id":"s","name":"S","works":[{"work":{"id":"w1","series":{"id":"s","name":"S","position":"7"}}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	feed, _, ok := NewClient(srv.URL).seriesListing(context.Background(), "s")
	if !ok {
		t.Fatal("decode failed")
	}
	if len(feed.entries) != 1 || feed.entries[0].Pos != "7" {
		t.Errorf("entries = %+v, want the nested position 7", feed.entries)
	}
}

func TestSeriesPriorForFindsThePredecessor(t *testing.T) {
	c, _ := newMeta(t, jackReacher())

	p, definitive, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
		WorkID: "die-trying", SeriesName: "Jack Reacher", SeriesPos: "2",
	})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if p.Empty() {
		t.Fatal("expected the predecessor volume's material")
	}
	if !definitive {
		t.Error("a positive result is always definitive")
	}
	if p.WorkID != "killing-floor" || p.Title != "Killing Floor" {
		t.Errorf("wrong predecessor: %q / %q", p.WorkID, p.Title)
	}
	if len(p.Authors) != 1 || p.Authors[0] != "Lee Child" {
		t.Errorf("authors = %v", p.Authors)
	}
	if !strings.Contains(p.InShort, "Margrave") || !strings.Contains(p.Ending, "conspiracy broken") {
		t.Errorf("recap_summary not carried: in_short=%q ending=%q", p.InShort, p.Ending)
	}
	// The FINAL chaptered recap is the one with the highest through.chapter, not the
	// last one listed: an out-of-order sidecar must not hand over a mid-book recap.
	if p.FinalRecapChapter != 30 || !strings.Contains(p.FinalRecap, "collapses") {
		t.Errorf("final recap = chapter %d %q", p.FinalRecapChapter, p.FinalRecap)
	}
}

// The book was never matched to an upstream work, so the series is resolved from the
// scanned series NAME. The slug is a guess, so it is trusted only when the fetched
// series' own name matches - attaching another series' recap would be a spoiler hazard.
func TestSeriesPriorForResolvesBySeriesNameWhenUnmatched(t *testing.T) {
	c, _ := newMeta(t, jackReacher())

	p, definitive, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{SeriesName: "Jack Reacher", SeriesPos: "2"})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if p.WorkID != "killing-floor" || !definitive {
		t.Errorf("work id = %q definitive = %v, want killing-floor/true", p.WorkID, definitive)
	}

	s := jackReacher()
	s.seriesName = "A Completely Different Series"
	c2, _ := newMeta(t, s)
	p, definitive, err = c2.SeriesPriorFor(context.Background(), SeriesPriorQuery{SeriesName: "Jack Reacher", SeriesPos: "2"})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if !p.Empty() {
		t.Errorf("a slug that landed on another series must not be trusted: %+v", p)
	}
	// The listing WAS read and is not this book's series: a settled "no prior".
	if !definitive {
		t.Error("a name mismatch on a successfully read listing is a settled answer")
	}
}

// A predecessor with no published recaps is SKIPPED, not returned empty-handed: the
// walk keeps going back to the nearest volume that actually has material.
func TestSeriesPriorForSkipsVolumesWithoutRecaps(t *testing.T) {
	s := jackReacher()
	s.work["tripwire"] = workRow{title: "Tripwire", seriesName: "Jack Reacher", seriesPos: "3"}
	s.seriesWorks["s"] = []string{"killing-floor", "die-trying", "tripwire"}

	c, _ := newMeta(t, s)
	p, definitive, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
		WorkID: "tripwire", SeriesName: "Jack Reacher", SeriesPos: "3",
	})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	// die-trying (position 2) has no recaps, so book 1's material is the prior.
	if p.WorkID != "killing-floor" || !definitive {
		t.Errorf("work id = %q definitive = %v, want killing-floor/true (die-trying publishes no recaps)", p.WorkID, definitive)
	}
}

// Every no-data path yields an empty result and a NIL error: a metadata outage must
// never park a book. Each also states whether that emptiness is DEFINITIVE - the
// caller persists a negative only when it is, so an outage can never freeze into
// "this book has no prior".
func TestSeriesPriorForDegradesQuietly(t *testing.T) {
	ctx := context.Background()
	q := SeriesPriorQuery{WorkID: "die-trying", SeriesName: "Jack Reacher", SeriesPos: "2"}

	cases := []struct {
		name string
		// client and query under test.
		build          func(t *testing.T) (*Client, SeriesPriorQuery)
		wantDefinitive bool
		why            string
	}{
		{
			name:  "disabled client",
			build: func(*testing.T) (*Client, SeriesPriorQuery) { return NewClient(""), q },
			why:   "metadata is off, so nothing was determined",
		},
		{
			name: "work has no series",
			build: func(t *testing.T) (*Client, SeriesPriorQuery) {
				s := jackReacher()
				s.work["standalone"] = workRow{title: "Standalone"}
				c, _ := newMeta(t, s)
				return c, SeriesPriorQuery{WorkID: "standalone"}
			},
			wantDefinitive: true,
			why:            "the work was read and has no series: settled",
		},
		{
			name: "no covered predecessor",
			build: func(t *testing.T) (*Client, SeriesPriorQuery) {
				c, _ := newMeta(t, jackReacher())
				// Book 1 is itself the opener: nothing sits below position 1.
				return c, SeriesPriorQuery{WorkID: "killing-floor", SeriesName: "Jack Reacher", SeriesPos: "1"}
			},
			wantDefinitive: true,
			why:            "the listing was walked to the end: settled",
		},
		{
			name: "series listing 404",
			build: func(t *testing.T) (*Client, SeriesPriorQuery) {
				s := jackReacher()
				s.seriesWorks = map[string][]string{}
				c, _ := newMeta(t, s)
				return c, q
			},
			wantDefinitive: true,
			why:            "upstream has no such series: settled",
		},
		{
			name: "predecessor unreachable",
			build: func(t *testing.T) (*Client, SeriesPriorQuery) {
				s := jackReacher()
				s.onWorks = func(id string) {
					if id == "killing-floor" {
						panic(http.ErrAbortHandler) // aborts this response only
					}
				}
				c, _ := newMeta(t, s)
				return c, q
			},
			why: "a transport failure leaves the volume unread",
		},
		{
			name: "series listing unreachable",
			build: func(t *testing.T) (*Client, SeriesPriorQuery) {
				s := jackReacher()
				s.onSeries = func(string) { panic(http.ErrAbortHandler) }
				c, _ := newMeta(t, s)
				return c, q
			},
			why: "the listing itself could not be read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, query := tc.build(t)
			p, definitive, err := c.SeriesPriorFor(ctx, query)
			if err != nil {
				t.Fatalf("no-data path must not error: %v", err)
			}
			if !p.Empty() {
				t.Fatalf("prior=%+v, want empty", p)
			}
			if definitive != tc.wantDefinitive {
				t.Errorf("definitive = %v, want %v (%s)", definitive, tc.wantDefinitive, tc.why)
			}
		})
	}
}

// The maxPriorFetches bound is a POLICY (volumes further back are no longer useful
// "previously" material), so stopping at it is a settled answer the caller may
// persist, not an incomplete walk it must retry forever.
func TestSeriesPriorForTruncatedWalkIsDefinitive(t *testing.T) {
	s := &metaServer{seriesName: "Long Series", work: map[string]workRow{}, seriesWorks: map[string][]string{}}
	var ids []string
	for i := 1; i <= maxPriorFetches+3; i++ {
		id := fmt.Sprintf("w%02d", i)
		ids = append(ids, id)
		// Only the very first volume publishes recaps, so it sits past the bound.
		row := workRow{title: id, seriesName: "Long Series", seriesPos: fmt.Sprint(i)}
		if i == 1 {
			row.recaps = []recapRow{{chapter: 10, text: "Book one ends."}}
		}
		s.work[id] = row
	}
	s.seriesWorks["s"] = ids
	last := ids[len(ids)-1]
	c, _ := newMeta(t, s)

	p, definitive, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
		WorkID: last, SeriesName: "Long Series", SeriesPos: fmt.Sprint(len(ids)),
	})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if !p.Empty() {
		t.Fatalf("the walk reached past its bound: %+v", p)
	}
	if !definitive {
		t.Error("a bounded walk that found nothing is a settled answer")
	}
}

// A transport failure on the IMMEDIATE predecessor stops the walk. Reaching past it
// would attach an earlier volume's "previously" to the book, which is worse than
// having none.
func TestSeriesPriorForDoesNotWalkPastAnUnreachableVolume(t *testing.T) {
	s := jackReacher()
	s.work["tripwire"] = workRow{title: "Tripwire", seriesName: "Jack Reacher", seriesPos: "3",
		recaps: []recapRow{{chapter: 40, text: "Tripwire ends."}}}
	s.work["running-blind"] = workRow{title: "Running Blind", seriesName: "Jack Reacher", seriesPos: "4"}
	s.seriesWorks["s"] = []string{"killing-floor", "die-trying", "tripwire", "running-blind"}
	s.onWorks = func(id string) {
		if id == "tripwire" {
			panic(http.ErrAbortHandler)
		}
	}
	c, _ := newMeta(t, s)

	p, definitive, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
		WorkID: "running-blind", SeriesName: "Jack Reacher", SeriesPos: "4",
	})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if !p.Empty() {
		t.Errorf("walked past an unreachable predecessor to %q", p.WorkID)
	}
	// And the stop is NOT a settled answer: the unread volume may well have recaps.
	if definitive {
		t.Error("a walk stopped by a transport failure must not be reported as definitive")
	}
}

// Finding 2's scenario: this book's own /works/{id} 502s AND the slug guess misses.
// Neither request settled anything, so the dead end must NOT be reported as
// definitive - a permanent {"none":true} on a transient outage is the exact failure
// the flag exists to prevent.
func TestSeriesPriorForUnreadableOwnWorkIsNotDefinitive(t *testing.T) {
	s := jackReacher()
	s.onWorks = func(id string) {
		if id == "die-trying" {
			panic(http.ErrAbortHandler)
		}
	}
	// The name fallback finds no such series either (the slug guesses both miss).
	s.seriesWorks = map[string][]string{}
	c, _ := newMeta(t, s)

	p, definitive, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
		WorkID: "die-trying", SeriesName: "Jack Reacher", SeriesPos: "2",
	})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if !p.Empty() {
		t.Fatalf("prior=%+v, want empty", p)
	}
	if definitive {
		t.Error("an unread own-work plus a slug miss is not a settled answer: a transient " +
			"outage would freeze a permanent negative")
	}
}

// Finding 3's scenario: earlier members are listed with OMNIBUS positions ("1-3"),
// which parseFloatSeq rejects. The position cut then yields nothing, and returning
// there made the listing-index cut unreachable - a permanent wrong negative.
func TestSeriesPriorForFallsBackWhenPositionsAreUnparseable(t *testing.T) {
	s := &metaServer{
		seriesName: "Omnibus Series",
		work: map[string]workRow{
			"omnibus-1-3": {title: "Books 1-3", seriesName: "Omnibus Series", seriesPos: "1-3",
				recaps: []recapRow{{chapter: 60, text: "The omnibus ends."}}},
			"book-4": {title: "Book Four", seriesName: "Omnibus Series", seriesPos: "4"},
		},
		seriesWorks: map[string][]string{"s": {"omnibus-1-3", "book-4"}},
	}
	c, _ := newMeta(t, s)

	p, definitive, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
		WorkID: "book-4", SeriesName: "Omnibus Series", SeriesPos: "4",
	})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if p.WorkID != "omnibus-1-3" {
		t.Errorf("work id = %q, want omnibus-1-3 (the listing-order cut must run when no "+
			"member states a comparable position)", p.WorkID)
	}
	if !definitive {
		t.Error("definitive = false for a completed walk")
	}
}

// Finding 4's scenario: match.NormalizeSeries strips leading articles but the ID must
// be GUESSED before anything is fetched, so "The Wandering Inn" has to try
// wandering-inn as well as the-wandering-inn or the fallback is dead for every
// "The ..." series.
func TestSeriesPriorForTriesBothSlugForms(t *testing.T) {
	for _, upstreamID := range []string{"the-wandering-inn", "wandering-inn"} {
		t.Run(upstreamID, func(t *testing.T) {
			s := &metaServer{
				seriesName: "The Wandering Inn",
				work: map[string]workRow{
					"book-1": {title: "Book One", seriesName: "The Wandering Inn", seriesPos: "1",
						recaps: []recapRow{{chapter: 12, text: "Erin opens the inn."}}},
				},
				seriesWorks: map[string][]string{upstreamID: {"book-1"}},
			}
			c, _ := newMeta(t, s)

			// No WorkID: the book was never matched, so only the name can resolve it.
			p, definitive, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
				SeriesName: "The Wandering Inn", SeriesPos: "2",
			})
			if err != nil {
				t.Fatalf("SeriesPriorFor: %v", err)
			}
			if p.WorkID != "book-1" {
				t.Errorf("work id = %q, want book-1 (upstream series id %q)", p.WorkID, upstreamID)
			}
			if !definitive {
				t.Error("definitive = false for a resolved series")
			}
		})
	}
}

// A predecessor's own scope:"series" recap summarises ITS predecessors, not its story.
// Staging it as that volume's final recap would break the promise the staged header
// makes about whose story the material describes.
func TestSeriesPriorForIgnoresTheScopeSeriesRecap(t *testing.T) {
	s := jackReacher()
	row := s.work["killing-floor"]
	// A higher-chaptered series recap would otherwise win on chapter number alone.
	row.recaps = append(row.recaps, recapRow{chapter: 99, scope: recapScopeSeries,
		text: "Previously, in the books before this one."})
	s.work["killing-floor"] = row
	c, _ := newMeta(t, s)

	p, _, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
		WorkID: "die-trying", SeriesName: "Jack Reacher", SeriesPos: "2",
	})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if p.FinalRecapChapter != 30 || strings.Contains(p.FinalRecap, "Previously") {
		t.Errorf("a scope:series recap was staged as the volume's own final recap: chapter %d %q",
			p.FinalRecapChapter, p.FinalRecap)
	}
}

func TestStripLeadingArticle(t *testing.T) {
	for in, want := range map[string]string{
		"The Wandering Inn":     "Wandering Inn",
		"A Deadly Education":    "Deadly Education",
		"An Ember in the Ashes": "Ember in the Ashes",
		"the expanse":           "expanse",
		"Mistborn":              "Mistborn",
		// Not an article, just a word that starts with one.
		"Theft of Swords": "Theft of Swords",
		"A":               "A",
	} {
		if got := stripLeadingArticle(in); got != want {
			t.Errorf("stripLeadingArticle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPriorCandidatesOrdersNearestFirst(t *testing.T) {
	entries := []seriesEntry{{ID: "a", Pos: "1"}, {ID: "b", Pos: "2"}, {ID: "c", Pos: "2.5"}, {ID: "d", Pos: "3"}}

	got := priorCandidates(entries, SeriesPriorQuery{SeriesPos: "3"})
	if len(got) != 3 || got[0] != "c" || got[1] != "b" || got[2] != "a" {
		t.Errorf("by position: %v", got)
	}

	// No parseable position: fall back to this book's index in the series listing.
	got = priorCandidates(entries, SeriesPriorQuery{WorkID: "d"})
	if len(got) != 3 || got[0] != "c" {
		t.Errorf("by listing index: %v", got)
	}

	// Neither a position nor a listed work id: no derivable predecessor.
	if got := priorCandidates(entries, SeriesPriorQuery{WorkID: "unlisted"}); got != nil {
		t.Errorf("want no candidates, got %v", got)
	}
}

func TestSeriesSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Jack Reacher":        "jack-reacher",
		"  The Wandering Inn": "the-wandering-inn",
		"Mistborn: Era 1":     "mistborn-era-1",
		"":                    "",
	} {
		if got := seriesSlug(in); got != want {
			t.Errorf("seriesSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
