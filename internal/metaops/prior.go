package metaops

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// The bounds on the predecessor walk. The material is staged into agent prompts, so
// it stays small and the lookup stays cheap.
const (
	// maxPriorFetches bounds how far back through a series the walk looks for a
	// volume that actually published recaps. Upstream sidecar coverage is sparse, so
	// the immediate predecessor is often uncontributed, but a recap from many volumes
	// back is no longer the "previously" a reader of this book needs.
	maxPriorFetches = 6
	// priorFetchBudget bounds the whole walk. Each request has its own timeout, but a
	// hung (rather than failing) upstream would otherwise stall the stage for minutes.
	// Prior material is evidence, not a precondition.
	priorFetchBudget = 30 * time.Second
)

// recapScopeSeries is the recaps sidecar's scope value for a "previously, in earlier
// books" entry (the schema's other value is "book").
const recapScopeSeries = "series"

// SeriesPrior is the community-published recap material of the NEAREST EARLIER volume
// in a book's series that has published recaps - the only legitimate source for that
// book's `chapter: 0` `scope: "series"` "previously" recap when the earlier volumes
// were never processed locally (they are already covered upstream, so nobody
// re-derives them).
//
// It is not necessarily the ADJACENT volume: upstream sidecar coverage is sparse, so
// the walk passes over nearer volumes that publish none. Consumers must therefore
// claim only what the material states and never present it as everything that
// happened before the book.
//
// The text is CC BY-SA 3.0 community writing from meta.audiosilo.app, not the novel.
type SeriesPrior struct {
	// WorkID/Title/Authors identify the predecessor volume the material came from.
	WorkID  string
	Title   string
	Authors []string
	// InShort and Ending are the predecessor's recap_summary: the whole arc in a
	// paragraph and the sequel-handoff state.
	InShort string
	Ending  string
	// FinalRecap is the text of the predecessor's last chaptered recap (the highest
	// through.chapter), with FinalRecapChapter the chapter it runs through.
	FinalRecap        string
	FinalRecapChapter int
}

// Empty reports whether no predecessor material was found.
func (p SeriesPrior) Empty() bool { return p.WorkID == "" }

// SeriesPriorQuery identifies the book whose predecessor is wanted. WorkID is this
// book's own upstream work id when it has one; SeriesName/SeriesPos are the local
// scan's series identity, which is all an unmatched book has.
type SeriesPriorQuery struct {
	WorkID     string
	SeriesName string
	SeriesPos  string
}

// SeriesPriorFor returns the published recap material of the nearest earlier volume
// in this book's series: the covered work with the highest series position strictly
// below this book's that HAS published recaps, within maxPriorFetches of it. Nearer
// volumes that publish none are passed over, so the result is not necessarily the
// adjacent volume.
//
// It is best-effort by design and mirrors CoverageFor's contract: a disabled client,
// a book with no series, an unreachable upstream, or a series whose earlier volumes
// carry no recaps sidecar all yield an empty result and a nil error. A metadata
// outage must never park a book - the caller persists the first successful result so
// a later outage cannot retract it.
//
// definitive reports whether an EMPTY result is a settled answer ("this book has no
// upstream prior") rather than a degraded one ("we could not find out"). It lets the
// caller persist a negative and stop re-walking the series on every stage entry.
// Settled: no series to resolve, a series that upstream does not have, a listing whose
// name is not this book's series, this book absent from the listing, and a completed
// walk in which no candidate published recaps. NOT settled: a disabled client, an
// unreachable series listing, a transport failure mid-walk, or a spent deadline - all
// of which are also the paths that must never freeze into a negative. A positive
// result is always definitive.
//
// A maxPriorFetches truncation COUNTS as definitive: the bound is a deliberate policy
// that volumes further back are no longer useful "previously" material, so "none
// within the bound" is the final answer, not an incomplete one.
func (c *Client) SeriesPriorFor(ctx context.Context, q SeriesPriorQuery) (prior SeriesPrior, definitive bool, err error) {
	if !c.Enabled() {
		return SeriesPrior{}, false, nil
	}

	// One deadline for the whole walk, for the same reason SeriesGlossary has one.
	ctx, cancel := context.WithTimeout(ctx, priorFetchBudget)
	defer cancel()

	entries, ok, definitive := c.resolveSeriesListing(ctx, q)
	if !ok {
		return SeriesPrior{}, definitive, nil
	}
	candidates := priorCandidates(entries, q)
	for i, id := range candidates {
		if i >= maxPriorFetches {
			break // a settled answer by policy; see the doc comment
		}
		if ctx.Err() != nil {
			return SeriesPrior{}, false, nil
		}
		prior, found, ok := c.priorMaterial(ctx, id)
		if !ok {
			// A transport failure means we cannot tell whether THIS volume has recaps.
			// Walking past it would attach an earlier volume's "previously" to the book,
			// which is worse than no material at all, so stop and degrade.
			return SeriesPrior{}, false, nil
		}
		if found {
			return prior, true, nil
		}
	}
	return SeriesPrior{}, true, nil
}

// resolveSeriesListing finds the series this book belongs to and returns its members
// in series order. ok=false means no series could be resolved; definitive then splits
// "there is no such series" from "we could not reach it" (see SeriesPriorFor).
func (c *Client) resolveSeriesListing(ctx context.Context, q SeriesPriorQuery) (entries []seriesEntry, ok, definitive bool) {
	// reachable stays false once a request fails outright, so a later dead end is
	// reported as unknown rather than settled.
	reachable := true
	if workID := strings.TrimSpace(q.WorkID); workID != "" {
		work, found, wok := c.workDetail(ctx, workID)
		switch {
		case !wok:
			reachable = false // could not read this book's own work
		case found && work.series != nil && work.series.ID != "":
			feed, sfound, sok := c.seriesListing(ctx, work.series.ID)
			if !sok {
				return nil, false, false
			}
			return feed.entries, sfound, true
		}
	}

	// The book was never matched to an upstream work (or its work records no series
	// membership), so derive the series id from the local series name. The derived id
	// is a GUESS, and attaching the wrong series' recap to a book is a spoiler hazard,
	// so it is trusted only when the fetched series' own name matches what the scan
	// read off this book.
	name := strings.TrimSpace(q.SeriesName)
	if name == "" {
		return nil, false, reachable
	}
	for _, slug := range seriesSlugCandidates(name) {
		feed, found, ok := c.seriesListing(ctx, slug)
		if !ok {
			reachable = false
			continue
		}
		if found && match.NormalizeSeries(feed.name) == match.NormalizeSeries(name) {
			return feed.entries, true, true
		}
	}
	// No candidate slug resolved to this book's series. That is settled ONLY if every
	// request along the way actually completed - a 502 on this book's own work or on a
	// candidate listing leaves the question open, and reporting it settled would let a
	// transient outage freeze a permanent "no prior".
	return nil, false, reachable
}

// seriesSlugCandidates derives the series ids to try for a scanned series name, in
// descending confidence and deduped: the plain slug, then the slug of the name with a
// leading article dropped.
//
// The second form is not optional. The NAME check above uses match.NormalizeSeries,
// which strips leading articles, but the ID has to be guessed BEFORE anything can be
// fetched - so a book scanned as "The Wandering Inn" guesses the-wandering-inn, 404s
// against an upstream id of wandering-inn, and the whole fallback is dead for every
// "The ..." series.
func seriesSlugCandidates(name string) []string {
	var out []string
	add := func(s string) {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	add(seriesSlug(name))
	add(seriesSlug(stripLeadingArticle(name)))
	return out
}

// stripLeadingArticle drops a leading English article from a series name.
func stripLeadingArticle(name string) string {
	name = strings.TrimSpace(name)
	for _, article := range []string{"the ", "an ", "a "} {
		if len(name) > len(article) && strings.EqualFold(name[:len(article)], article) {
			return strings.TrimSpace(name[len(article):])
		}
	}
	return name
}

// priorCandidates returns the series members that precede this book, NEAREST FIRST.
//
// The cut is by series position when the book states a parseable one, and otherwise
// (or when no member states a comparable position) by this book's index in the
// listing, which upstream returns in series order. A book in neither has no derivable
// predecessor.
func priorCandidates(entries []seriesEntry, q SeriesPriorQuery) []string {
	workID := strings.TrimSpace(q.WorkID)
	if pos, ok := parseFloatSeq(q.SeriesPos); ok {
		var out []string
		for _, e := range entries {
			if p, ok := parseFloatSeq(e.Pos); ok && p < pos {
				out = append(out, e.ID)
			}
		}
		// An EMPTY result here is not an answer, it is an unusable position column:
		// real listings record omnibus members as ranges ("1-3"), which parseFloatSeq
		// rejects, so every earlier volume is skipped and the walk never runs. Fall
		// through to the listing-order cut rather than reporting "no predecessor".
		if len(out) > 0 || workID == "" {
			slices.Reverse(out)
			return out
		}
	}
	if workID == "" {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.ID == workID {
			slices.Reverse(out)
			return out
		}
		out = append(out, e.ID)
	}
	return nil // this book is not in the listing: no cut point, so no predecessor
}

// priorMaterial builds one candidate's SeriesPrior. found=false means the volume
// publishes no usable recaps (skip it); ok=false is a transport failure (stop).
func (c *Client) priorMaterial(ctx context.Context, workID string) (SeriesPrior, bool, bool) {
	work, found, ok := c.workDetail(ctx, workID)
	if !ok {
		return SeriesPrior{}, false, false
	}
	if !found {
		return SeriesPrior{}, false, true
	}
	p := SeriesPrior{
		WorkID:  workID,
		Title:   work.title,
		Authors: work.authors,
		InShort: strings.TrimSpace(work.inShort),
		Ending:  strings.TrimSpace(work.ending),
	}
	for _, rc := range work.recaps {
		// scope:"series" is that volume's OWN "previously" recap, summarising ITS
		// predecessors rather than its story - usually at chapter 0, but a long series
		// can carry a chaptered one. Either way it is the wrong book: staging it as
		// this volume's final recap would break the promise the staged header makes
		// about whose story the material describes.
		if rc.scope == recapScopeSeries {
			continue
		}
		if rc.chapter > p.FinalRecapChapter && strings.TrimSpace(rc.text) != "" {
			p.FinalRecap = strings.TrimSpace(rc.text)
			p.FinalRecapChapter = rc.chapter
		}
	}
	if p.InShort == "" && p.Ending == "" && p.FinalRecap == "" {
		return SeriesPrior{}, false, true
	}
	return p, true, true
}

// seriesSlug derives a meta series id from a series name ("Jack Reacher" ->
// "jack-reacher"). The result is only ever used as a guess that resolveSeriesListing
// confirms against the fetched series' name.
func seriesSlug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
