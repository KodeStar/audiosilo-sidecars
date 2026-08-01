package metaops

import (
	"context"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
)

// The series-glossary bounds. A glossary is staged into an agent prompt and into
// the correction gate's attestation corpus, so it stays small and predictable
// rather than growing with a 14-volume series.
const (
	// maxGlossarySiblings caps how many sibling works are fetched for one glossary.
	// Ordered by series position, so the cap drops the far end of a long series
	// rather than an arbitrary slice. It counts volumes that actually CONTRIBUTED a
	// name: sidecar coverage upstream is sparse, and counting empty volumes against
	// the budget would let a long series whose early books carry no sidecars spend
	// the whole cap on them and produce an empty glossary.
	maxGlossarySiblings = 12
	// maxGlossaryFetches bounds the work when most volumes contribute nothing, since
	// those no longer consume the sibling cap.
	maxGlossaryFetches = 24
	// maxGlossaryNames caps the emitted name count.
	maxGlossaryNames = 1500
	// minGlossaryName is the shortest name worth carrying. One- and two-character
	// forms ("Al", "Xi") cannot be matched safely by edit distance and would only
	// add noise.
	minGlossaryName = 3
	// glossaryFetchBudget bounds the whole sibling fan-out. Each request has its
	// own timeout, but up to 14 sequential requests against a hung upstream would
	// otherwise stall the stage for minutes.
	glossaryFetchBudget = 60 * time.Second
)

// Glossary is the canonical character vocabulary for a work's series, drawn from
// the OTHER volumes' community-accepted characters sidecars.
//
// It is deliberately NOT the work's own sidecar: this is the prior art a book is
// checked against, and a book has not been contributed yet when it runs.
type Glossary struct {
	// SeriesName is the series the names came from ("" when the work has no series
	// membership upstream).
	SeriesName string
	// Works are the sibling work ids actually consulted, sorted. Lines() emits them
	// as comments, so the written reference file records its own provenance.
	Works []string
	// Names are the canonical names, sorted and deduped across the siblings.
	Names []string
	// Truncated counts names dropped by maxGlossaryNames (0 = none). The caller
	// surfaces it in a stage note, so a drop is never silent.
	Truncated int
}

// Empty reports whether the glossary carries no usable names.
func (g Glossary) Empty() bool { return len(g.Names) == 0 }

// Lines renders the glossary as the plain-text reference file the correction gate
// attests against: one name per line, sorted, with a leading comment naming the
// provenance. readReferenceSource reads it verbatim, so the comment is harmless
// context (it can only ever ADD attested words, and the words it adds are the
// series name and work slugs).
func (g Glossary) Lines() string {
	var b strings.Builder
	b.WriteString("# canonical names from the community metadata database\n")
	if g.SeriesName != "" {
		b.WriteString("# series: " + g.SeriesName + "\n")
	}
	for _, w := range g.Works {
		b.WriteString("# work: " + w + "\n")
	}
	for _, n := range g.Names {
		b.WriteString(n + "\n")
	}
	return b.String()
}

// SeriesGlossary returns the canonical character names recorded upstream for the
// other works in workID's series.
//
// It is best-effort by design and mirrors CoverageFor's contract: a disabled
// client, a work with no series, an unreachable upstream, or siblings without
// characters sidecars all yield an empty glossary and a nil error. The caller
// treats "no glossary" as "no extra evidence", never as a failure - a metadata
// outage must not park a book.
func (c *Client) SeriesGlossary(ctx context.Context, workID string) (Glossary, error) {
	workID = strings.TrimSpace(workID)
	if !c.Enabled() || workID == "" {
		return Glossary{}, nil
	}
	if cached, hit := c.glossaries.get(workID); hit {
		return cached, nil
	}

	// One deadline for the whole fan-out. Each request already has its own timeout,
	// but up to 14 of them in sequence means a hung (rather than failing) upstream
	// could stall the stage for minutes before degrading to "no glossary". The
	// glossary is evidence, not a precondition, so it is not worth waiting for.
	ctx, cancel := context.WithTimeout(ctx, glossaryFetchBudget)
	defer cancel()

	work, found, ok := c.workDetail(ctx, workID)
	if !ok || !found || work.series == nil || work.series.ID == "" {
		return Glossary{}, nil
	}

	siblings, ok := c.seriesWorks(ctx, work.series.ID)
	if !ok {
		return Glossary{}, nil
	}

	// Only volumes EARLIER in the series are consulted. A later volume's canonical
	// can suppress the very proposal this feature exists to make: if book 9
	// introduces a character genuinely named "Floss", that name settles the form and
	// book 1's misheard "Floss" (for "Flos") is never questioned. Series listings
	// come back in position order, so the position of this work in the list is the
	// cut - no position parsing needed.
	if i := slices.Index(siblings, workID); i >= 0 {
		siblings = siblings[:i]
	}

	g := Glossary{SeriesName: work.series.Name}
	names := map[string]bool{}
	consulted := map[string]bool{}
	fetched := 0
	// degraded records that a sibling could not be READ (transport failure, 5xx,
	// per-request timeout), as opposed to having no characters sidecar. The
	// difference decides whether this glossary may be cached: a volume with no
	// sidecar is a settled fact, a volume we failed to reach is not, and caching a
	// glossary that is short because of a transient 502 would hide the missing names
	// for the full TTL - including across a Retry.
	degraded := false
	for _, sib := range siblings {
		if sib == workID || sib == "" {
			continue
		}
		if len(consulted) >= maxGlossarySiblings || fetched >= maxGlossaryFetches {
			break
		}
		fetched++
		sibNames, found, ok := c.workCharacterNames(ctx, sib)
		if !ok || !found {
			// One unreachable sibling never fails the glossary, but it does mark it
			// incomplete. Once the whole-fan-out budget is spent every remaining
			// sibling fails identically, so stop rather than burn the list.
			degraded = true
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if len(sibNames) == 0 {
			continue // a volume with no characters sidecar costs no sibling budget
		}
		consulted[sib] = true
		for _, n := range sibNames {
			names[n] = true
		}
	}

	g.Works = slices.Sorted(maps.Keys(consulted))
	g.Names = slices.Sorted(maps.Keys(names))
	if len(g.Names) > maxGlossaryNames {
		g.Truncated = len(g.Names) - maxGlossaryNames
		g.Names = g.Names[:maxGlossaryNames]
	}

	// Cache only a glossary that ran to completion. A volume with no characters
	// sidecar is a settled, cacheable fact; an unread volume is not, because it means
	// names are MISSING and remembering that for an hour would hide the gap from
	// every retry in the window. The partial result is still returned - better
	// evidence than none for this run - just not remembered.
	if !degraded && ctx.Err() == nil {
		c.glossaries.put(workID, g)
	}
	return g, nil
}

// seriesEntry is one member of a series listing: the work id and its position in
// the series ("1", "2.5"). Position is empty when upstream records none.
type seriesEntry struct {
	ID  string
	Pos string
}

// seriesFeedVal is a cached series listing: the series' own name plus its members
// in series order. The name is cached because the prior-material fallback resolves
// a series by SLUGIFYING a local series name and must confirm the guess landed on
// the right series before trusting its recaps.
type seriesFeedVal struct {
	name    string
	entries []seriesEntry
}

// seriesWorks fetches a series' member work ids in series order. ok=false is a
// transport failure or a clean 404.
func (c *Client) seriesWorks(ctx context.Context, seriesID string) ([]string, bool) {
	feed, ok := c.seriesListing(ctx, seriesID)
	if !ok {
		return nil, false
	}
	ids := make([]string, 0, len(feed.entries))
	for _, e := range feed.entries {
		ids = append(ids, e.ID)
	}
	return ids, true
}

// seriesListing fetches (and caches) a series' name and members in series order.
// ok=false is a transport failure or a clean 404.
func (c *Client) seriesListing(ctx context.Context, seriesID string) (seriesFeedVal, bool) {
	if cached, hit := c.seriesFeed.get(seriesID); hit {
		return cached, true
	}
	var res struct {
		Name  string `json:"name"`
		Works []struct {
			Position string `json:"position"`
			Work     *struct {
				ID     string `json:"id"`
				Series []struct {
					Position string `json:"position"`
				} `json:"series"`
			} `json:"work"`
		} `json:"works"`
	}
	found, ok := c.getJSON(ctx, "/api/v1/series/"+url.PathEscape(seriesID), &res)
	if !ok || !found {
		return seriesFeedVal{}, false
	}
	feed := seriesFeedVal{name: res.Name, entries: make([]seriesEntry, 0, len(res.Works))}
	for _, e := range res.Works {
		if e.Work == nil || e.Work.ID == "" {
			continue
		}
		pos := e.Position
		// The nested work echoes its own membership; it is the fallback when the
		// listing entry itself records no position.
		if pos == "" && len(e.Work.Series) > 0 {
			pos = e.Work.Series[0].Position
		}
		feed.entries = append(feed.entries, seriesEntry{ID: e.Work.ID, Pos: pos})
	}
	c.seriesFeed.put(seriesID, feed)
	return feed, true
}

// workCharacterNames returns one work's usable character names and aliases. It
// reads through workDetail, so a sibling already fetched by the coverage path costs
// nothing and the works/{id} payload stays mirrored in exactly one place.
//
// It mirrors workDetail's tri-state: ok=false is a transport failure, found=false a
// clean 404. The caller needs them apart - one means "this volume has no names",
// the other means "we could not find out", and only the first may be cached.
func (c *Client) workCharacterNames(ctx context.Context, workID string) (names []string, found, ok bool) {
	work, found, ok := c.workDetail(ctx, workID)
	if !ok || !found {
		return nil, found, ok
	}
	for _, n := range work.charNames {
		if n = strings.TrimSpace(n); usableGlossaryName(n) {
			names = append(names, n)
		}
	}
	return names, true, true
}

// usableGlossaryName filters the descriptive placeholder labels a sidecar uses for
// an unnamed figure ("The missing young gnoll child", "the necromancer"). Those are
// prose, not spellings: matching a transcript form against them would propose
// nonsense corrections. A usable name starts with an uppercase letter and is not a
// lowercase-word phrase.
func usableGlossaryName(n string) bool {
	if len([]rune(n)) < minGlossaryName {
		return false
	}
	if !nameShaped(n) {
		return false
	}
	// A descriptive label reads as ordinary prose: several words, only the first
	// capitalized. A real multi-part name capitalizes its parts ("Laken Godart",
	// "Az'kerash", "Garen Redfang").
	fields := strings.Fields(n)
	if len(fields) < 2 {
		return true
	}
	for _, f := range fields[1:] {
		if nameShaped(f) {
			return true // at least one further name-shaped part
		}
	}
	return false
}

// nameShaped reports whether a word looks like a name rather than prose.
//
// It deliberately does not test `unicode.IsUpper(first)`. That drops the d'Aston
// shape - lowercase first letter, capital after an internal apostrophe - which is
// the exact pitfall internal/spelling documents (its isCapitalized handles it, and
// its candidate extractor emits that token shape), so rejecting it here would leave
// the matcher with no canonical to compare a misheard "d'Daston" against. It also
// drops every uncased script. The test is therefore "not lowercase", which admits
// both, while still rejecting the prose labels this filter exists for.
//
// This duplicates spelling.isCapitalized's intent rather than calling it: metaops
// depends only on stdlib, the meta module and pkg/match, and reaching into
// internal/spelling for a ten-line predicate is not worth widening that.
func nameShaped(w string) bool {
	r := []rune(w)
	if len(r) == 0 || unicode.IsLower(r[0]) {
		// A lowercase lead is prose, UNLESS a capital follows an internal
		// apostrophe ("d'Aston").
		for i := 0; i+1 < len(r); i++ {
			if r[i] == '\'' && unicode.IsUpper(r[i+1]) {
				return true
			}
		}
		return false
	}
	return true
}
