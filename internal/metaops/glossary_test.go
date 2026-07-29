package metaops

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// wanderingInn builds a fake series shaped like the real case this feature exists
// for: the book being processed (w-book6) plus sibling volumes whose contributed
// sidecars carry the canonical spellings.
func wanderingInn() *metaServer {
	return &metaServer{
		work: map[string]workRow{
			"w-book6": {title: "The General of Izril", seriesName: "The Wandering Inn", seriesPos: "6"},
			"w-book5": {title: "The Last Light", seriesName: "The Wandering Inn", seriesPos: "5",
				chars: []string{"Toren", "Flos", "Teriarch"}},
			"w-book4": {title: "Flowers of Esthelm", seriesName: "The Wandering Inn", seriesPos: "4",
				chars: []string{"Toren", "Laken Godart"}},
		},
		seriesWorks: map[string][]string{"s": {"w-book4", "w-book5", "w-book6"}},
	}
}

func TestSeriesGlossaryCollectsSiblingNames(t *testing.T) {
	c, _ := newMeta(t, wanderingInn())

	g, err := c.SeriesGlossary(context.Background(), "w-book6")
	if err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	if g.Empty() {
		t.Fatal("expected names from the sibling volumes")
	}
	for _, want := range []string{"Toren", "Flos", "Teriarch", "Laken Godart"} {
		if !slices.Contains(g.Names, want) {
			t.Errorf("missing canonical name %q (got %v)", want, g.Names)
		}
	}
	// A name appearing in two volumes is deduped, and the list is sorted.
	if !slices.IsSorted(g.Names) {
		t.Errorf("names must be sorted: %v", g.Names)
	}
	if n := slices.Index(g.Names, "Toren"); n >= 0 && slices.Contains(g.Names[n+1:], "Toren") {
		t.Errorf("Toren appears twice: %v", g.Names)
	}
	// The consulted volumes are recorded, sorted - Lines() emits them as comments,
	// so the written reference file carries its own provenance.
	if !slices.Equal(g.Works, []string{"w-book4", "w-book5"}) {
		t.Errorf("consulted works = %v, want [w-book4 w-book5]", g.Works)
	}
	if g.SeriesName != "The Wandering Inn" {
		t.Errorf("series name = %q", g.SeriesName)
	}
}

// TestSeriesGlossaryExcludesTheBookItself: the work being processed has not been
// contributed yet, and if it ever were, checking a book against its own sidecar
// would make its mistakes self-attesting.
func TestSeriesGlossaryExcludesTheBookItself(t *testing.T) {
	s := wanderingInn()
	s.work["w-book6"] = workRow{title: "The General of Izril", seriesName: "The Wandering Inn",
		seriesPos: "6", chars: []string{"Torrin"}}
	c, _ := newMeta(t, s)

	g, err := c.SeriesGlossary(context.Background(), "w-book6")
	if err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	if slices.Contains(g.Names, "Torrin") {
		t.Fatal("the book's own characters must never enter its glossary")
	}
	for _, w := range g.Works {
		if w == "w-book6" {
			t.Fatal("the book itself must not be listed as a consulted work")
		}
	}
}

// TestSeriesGlossaryDropsDescriptiveLabels: a sidecar names an unnamed figure with
// a prose label. Those are not spellings, and matching transcript forms against
// them would propose nonsense.
func TestSeriesGlossaryDropsDescriptiveLabels(t *testing.T) {
	s := wanderingInn()
	s.work["w-book5"] = workRow{title: "The Last Light", seriesName: "The Wandering Inn", seriesPos: "5",
		chars: []string{"Toren", "The missing young gnoll child", "An unnamed skeleton warrior", "Al"}}
	c, _ := newMeta(t, s)

	g, err := c.SeriesGlossary(context.Background(), "w-book6")
	if err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	for _, n := range g.Names {
		switch n {
		case "The missing young gnoll child", "An unnamed skeleton warrior":
			t.Errorf("descriptive label %q must not enter the glossary", n)
		case "Al":
			t.Error("a two-character name is too short to match safely")
		}
	}
}

// TestSeriesGlossaryDegradesQuietly: every no-data path yields an empty glossary and
// a nil error. A metadata outage must never park a book.
func TestSeriesGlossaryDegradesQuietly(t *testing.T) {
	t.Run("disabled client", func(t *testing.T) {
		g, err := NewClient("").SeriesGlossary(context.Background(), "w-book6")
		if err != nil || !g.Empty() {
			t.Errorf("got (%+v, %v), want empty and nil", g, err)
		}
	})
	t.Run("unknown work", func(t *testing.T) {
		c, _ := newMeta(t, wanderingInn())
		g, err := c.SeriesGlossary(context.Background(), "nope")
		if err != nil || !g.Empty() {
			t.Errorf("got (%+v, %v), want empty and nil", g, err)
		}
	})
	t.Run("work with no series", func(t *testing.T) {
		c, _ := newMeta(t, &metaServer{work: map[string]workRow{"solo": {title: "Standalone"}}})
		g, err := c.SeriesGlossary(context.Background(), "solo")
		if err != nil || !g.Empty() {
			t.Errorf("got (%+v, %v), want empty and nil", g, err)
		}
	})
	t.Run("series not found", func(t *testing.T) {
		s := wanderingInn()
		s.seriesWorks = nil
		c, _ := newMeta(t, s)
		g, err := c.SeriesGlossary(context.Background(), "w-book6")
		if err != nil || !g.Empty() {
			t.Errorf("got (%+v, %v), want empty and nil", g, err)
		}
	})
	t.Run("empty work id", func(t *testing.T) {
		c, _ := newMeta(t, wanderingInn())
		g, err := c.SeriesGlossary(context.Background(), "  ")
		if err != nil || !g.Empty() {
			t.Errorf("got (%+v, %v), want empty and nil", g, err)
		}
	})
}

// TestSeriesGlossaryDoesNotCacheAPartialResult: the fan-out runs under one deadline,
// and once it is spent every remaining sibling fails identically. Caching what was
// collected up to that point would hide the missing names for a full TTL, so a book
// retried inside the hour would silently reuse a short glossary.
func TestSeriesGlossaryDoesNotCacheAPartialResult(t *testing.T) {
	s := wanderingInn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cut the fan-out off after the FIRST sibling is served, so w-book4's names are
	// collected and w-book5's are not. Deterministic - no sleeps, no real deadline.
	// Read on the server goroutine, so it must be atomic.
	var cutOff atomic.Bool
	cutOff.Store(true)
	s.onWorks = func(id string) {
		if id == "w-book4" && cutOff.Load() {
			cancel()
		}
	}
	c, _ := newMeta(t, s)

	partial, err := c.SeriesGlossary(ctx, "w-book6")
	if err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	if slices.Contains(partial.Names, "Teriarch") {
		t.Fatal("the fan-out was not actually interrupted; this test proves nothing")
	}

	// A healthy call afterwards must redo the work rather than serve the truncated
	// result out of the cache.
	cutOff.Store(false)
	g, err := c.SeriesGlossary(context.Background(), "w-book6")
	if err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	for _, want := range []string{"Toren", "Flos", "Teriarch", "Laken Godart"} {
		if !slices.Contains(g.Names, want) {
			t.Fatalf("a partial glossary was cached: %q missing from %v", want, g.Names)
		}
	}
}

// TestSeriesGlossaryDoesNotCacheAfterAFailedSibling: a transient 502 (metaserve
// hot-swaps its artifact on every data release) drops one volume's names. Caching
// that would hide them for the full hour, including across a Retry - the failure
// this feature exists to prevent, reintroduced one layer down.
func TestSeriesGlossaryDoesNotCacheAfterAFailedSibling(t *testing.T) {
	s := wanderingInn()
	// The flag is read on the server goroutine, so it must be atomic.
	var fail atomic.Bool
	fail.Store(true)
	s.onWorks = func(id string) {
		if id == "w-book5" && fail.Load() {
			panic(http.ErrAbortHandler) // aborts this response only
		}
	}
	c, _ := newMeta(t, s)

	first, err := c.SeriesGlossary(context.Background(), "w-book6")
	if err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	if slices.Contains(first.Names, "Teriarch") {
		t.Fatal("w-book5 did not actually fail; this test proves nothing")
	}

	// Upstream recovers: the next call must go back out rather than serve the
	// short glossary from the cache.
	fail.Store(false)
	g, err := c.SeriesGlossary(context.Background(), "w-book6")
	if err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	if !slices.Contains(g.Names, "Teriarch") {
		t.Errorf("a glossary short of a failed volume was cached: %v", g.Names)
	}
}

// TestSeriesGlossarySkipsVolumesWithNoSidecar: sidecar coverage upstream is sparse.
// If volumes without one consumed the sibling budget, a long series whose early
// books are uncontributed would spend the whole cap on them and return nothing -
// exactly the long series this feature is for.
func TestSeriesGlossarySkipsVolumesWithNoSidecar(t *testing.T) {
	s := &metaServer{work: map[string]workRow{}, seriesWorks: map[string][]string{"s": {}}}
	var ids []string
	for i := 1; i <= 15; i++ {
		id := fmt.Sprintf("w%02d", i)
		ids = append(ids, id)
		row := workRow{title: id, seriesName: "Long Series", seriesPos: fmt.Sprint(i)}
		if i == 13 || i == 14 {
			row.chars = []string{"Canonical Name"}
		}
		s.work[id] = row
	}
	s.work["w15"] = workRow{title: "w15", seriesName: "Long Series", seriesPos: "15"}
	s.seriesWorks["s"] = ids
	c, _ := newMeta(t, s)

	g, err := c.SeriesGlossary(context.Background(), "w15")
	if err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	if !slices.Contains(g.Names, "Canonical Name") {
		t.Errorf("the only contributed volumes were skipped by the cap: names=%v works=%v", g.Names, g.Works)
	}
}

// TestSeriesGlossaryConsultsOnlyEarlierVolumes: a later volume's canonical settles
// the form and silences the proposal. If book 9 has a character genuinely named
// "Floss", book 6 must not learn that name, or its own misheard "Floss" (for
// "Flos") stops being questioned.
func TestSeriesGlossaryConsultsOnlyEarlierVolumes(t *testing.T) {
	s := wanderingInn()
	s.work["w-book9"] = workRow{title: "Book Nine", seriesName: "The Wandering Inn", seriesPos: "9",
		chars: []string{"Later Character"}}
	s.seriesWorks["s"] = []string{"w-book4", "w-book5", "w-book6", "w-book9"}
	c, _ := newMeta(t, s)

	g, err := c.SeriesGlossary(context.Background(), "w-book6")
	if err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	if slices.Contains(g.Names, "Later Character") {
		t.Error("a later volume's names must not enter an earlier book's glossary")
	}
	if !slices.Contains(g.Names, "Toren") {
		t.Errorf("earlier volumes must still be consulted: %v", g.Names)
	}
}

func TestUsableGlossaryName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Toren", true},
		{"Laken Godart", true},
		{"Az'kerash", true},
		// The d'Aston shape: a first-rune uppercase test would drop it, leaving the
		// matcher no canonical to compare the transcript's "d'Daston" against.
		{"d'Aston", true},
		{"d'Artagnan", true},
		{"The missing young gnoll child", false},
		{"the necromancer", false},
		{"Al", false},
	}
	for _, tc := range cases {
		if got := usableGlossaryName(tc.name); got != tc.want {
			t.Errorf("usableGlossaryName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSeriesGlossaryCaches(t *testing.T) {
	s := wanderingInn()
	c, _ := newMeta(t, s)
	ctx := context.Background()

	if _, err := c.SeriesGlossary(ctx, "w-book6"); err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	first := s.reqCount("works")
	if _, err := c.SeriesGlossary(ctx, "w-book6"); err != nil {
		t.Fatalf("SeriesGlossary: %v", err)
	}
	if got := s.reqCount("works"); got != first {
		t.Errorf("second call issued %d more work requests; want a cache hit", got-first)
	}
}

func TestGlossaryLines(t *testing.T) {
	g := Glossary{
		SeriesName: "The Wandering Inn",
		Works:      []string{"w-book4"},
		Names:      []string{"Toren", "Laken Godart"},
	}
	out := g.Lines()
	for _, want := range []string{"Toren\n", "Laken Godart\n", "# series: The Wandering Inn", "# work: w-book4"} {
		if !strings.Contains(out, want) {
			t.Errorf("Lines() missing %q:\n%s", want, out)
		}
	}
}
