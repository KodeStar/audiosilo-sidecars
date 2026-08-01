package metaops

import (
	"context"
	"net/http"
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

func TestSeriesPriorForFindsThePredecessor(t *testing.T) {
	c, _ := newMeta(t, jackReacher())

	p, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
		WorkID: "die-trying", SeriesName: "Jack Reacher", SeriesPos: "2",
	})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if p.Empty() {
		t.Fatal("expected the predecessor volume's material")
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

	p, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{SeriesName: "Jack Reacher", SeriesPos: "2"})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if p.WorkID != "killing-floor" {
		t.Errorf("work id = %q, want killing-floor", p.WorkID)
	}

	s := jackReacher()
	s.seriesName = "A Completely Different Series"
	c2, _ := newMeta(t, s)
	p, err = c2.SeriesPriorFor(context.Background(), SeriesPriorQuery{SeriesName: "Jack Reacher", SeriesPos: "2"})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if !p.Empty() {
		t.Errorf("a slug that landed on another series must not be trusted: %+v", p)
	}
}

// A predecessor with no published recaps is SKIPPED, not returned empty-handed: the
// walk keeps going back to the nearest volume that actually has material.
func TestSeriesPriorForSkipsVolumesWithoutRecaps(t *testing.T) {
	s := jackReacher()
	s.work["tripwire"] = workRow{title: "Tripwire", seriesName: "Jack Reacher", seriesPos: "3"}
	s.seriesWorks["s"] = []string{"killing-floor", "die-trying", "tripwire"}

	c, _ := newMeta(t, s)
	p, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
		WorkID: "tripwire", SeriesName: "Jack Reacher", SeriesPos: "3",
	})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	// die-trying (position 2) has no recaps, so book 1's material is the prior.
	if p.WorkID != "killing-floor" {
		t.Errorf("work id = %q, want killing-floor (die-trying publishes no recaps)", p.WorkID)
	}
}

// Every no-data path yields an empty result and a NIL error: a metadata outage must
// never park a book.
func TestSeriesPriorForDegradesQuietly(t *testing.T) {
	ctx := context.Background()
	q := SeriesPriorQuery{WorkID: "die-trying", SeriesName: "Jack Reacher", SeriesPos: "2"}

	t.Run("disabled client", func(t *testing.T) {
		p, err := NewClient("").SeriesPriorFor(ctx, q)
		if err != nil || !p.Empty() {
			t.Fatalf("prior=%+v err=%v", p, err)
		}
	})
	t.Run("no series at all", func(t *testing.T) {
		s := jackReacher()
		s.work["standalone"] = workRow{title: "Standalone"}
		c, _ := newMeta(t, s)
		p, err := c.SeriesPriorFor(ctx, SeriesPriorQuery{WorkID: "standalone"})
		if err != nil || !p.Empty() {
			t.Fatalf("prior=%+v err=%v", p, err)
		}
	})
	t.Run("no covered predecessor", func(t *testing.T) {
		c, _ := newMeta(t, jackReacher())
		// Book 1 is itself the opener: nothing sits below position 1.
		p, err := c.SeriesPriorFor(ctx, SeriesPriorQuery{
			WorkID: "killing-floor", SeriesName: "Jack Reacher", SeriesPos: "1",
		})
		if err != nil || !p.Empty() {
			t.Fatalf("prior=%+v err=%v", p, err)
		}
	})
	t.Run("predecessor unreachable", func(t *testing.T) {
		s := jackReacher()
		s.onWorks = func(id string) {
			if id == "killing-floor" {
				panic(http.ErrAbortHandler) // aborts this response only
			}
		}
		c, _ := newMeta(t, s)
		p, err := c.SeriesPriorFor(ctx, q)
		if err != nil {
			t.Fatalf("a transport failure must not error: %v", err)
		}
		if !p.Empty() {
			t.Fatalf("prior=%+v", p)
		}
	})
	t.Run("series listing 404", func(t *testing.T) {
		s := jackReacher()
		s.seriesWorks = map[string][]string{}
		c, _ := newMeta(t, s)
		p, err := c.SeriesPriorFor(ctx, q)
		if err != nil || !p.Empty() {
			t.Fatalf("prior=%+v err=%v", p, err)
		}
	})
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

	p, err := c.SeriesPriorFor(context.Background(), SeriesPriorQuery{
		WorkID: "running-blind", SeriesName: "Jack Reacher", SeriesPos: "4",
	})
	if err != nil {
		t.Fatalf("SeriesPriorFor: %v", err)
	}
	if !p.Empty() {
		t.Errorf("walked past an unreachable predecessor to %q", p.WorkID)
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
