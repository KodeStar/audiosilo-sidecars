package metaops

import (
	"context"
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

// SeriesPrior is the community-published recap material of the volume immediately
// before a book in its series - the only legitimate source for that book's
// `chapter: 0` `scope: "series"` "previously" recap when the predecessor was never
// processed locally (it is already covered upstream, so nobody re-derives it).
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

// SeriesPriorFor returns the published recap material of the volume immediately
// preceding this book in its series: the covered work with the highest series
// position strictly below this book's that HAS published recaps.
//
// It is best-effort by design and mirrors CoverageFor's contract: a disabled client,
// a book with no series, an unreachable upstream, or a series whose earlier volumes
// carry no recaps sidecar all yield an empty result and a nil error. A metadata
// outage must never park a book - the caller persists the first successful result so
// a later outage cannot retract it.
func (c *Client) SeriesPriorFor(ctx context.Context, q SeriesPriorQuery) (SeriesPrior, error) {
	if !c.Enabled() {
		return SeriesPrior{}, nil
	}

	// One deadline for the whole walk, for the same reason SeriesGlossary has one.
	ctx, cancel := context.WithTimeout(ctx, priorFetchBudget)
	defer cancel()

	entries, ok := c.resolveSeriesListing(ctx, q)
	if !ok {
		return SeriesPrior{}, nil
	}
	candidates := priorCandidates(entries, q)
	for i, id := range candidates {
		if i >= maxPriorFetches || ctx.Err() != nil {
			break
		}
		prior, found, ok := c.priorMaterial(ctx, id)
		if !ok {
			// A transport failure means we cannot tell whether THIS volume has recaps.
			// Walking past it would attach an earlier volume's "previously" to the book,
			// which is worse than no material at all, so stop and degrade.
			break
		}
		if found {
			return prior, nil
		}
	}
	return SeriesPrior{}, nil
}

// resolveSeriesListing finds the series this book belongs to and returns its members
// in series order. ok=false means no series could be resolved (or it was unreachable).
func (c *Client) resolveSeriesListing(ctx context.Context, q SeriesPriorQuery) ([]seriesEntry, bool) {
	if workID := strings.TrimSpace(q.WorkID); workID != "" {
		work, found, ok := c.workDetail(ctx, workID)
		if ok && found && work.series != nil && work.series.ID != "" {
			feed, ok := c.seriesListing(ctx, work.series.ID)
			return feed.entries, ok
		}
	}

	// The book was never matched to an upstream work (or its work records no series
	// membership), so derive the series id from the local series name. The derived id
	// is a GUESS, and attaching the wrong series' recap to a book is a spoiler hazard,
	// so it is trusted only when the fetched series' own name matches what the scan
	// read off this book.
	name := strings.TrimSpace(q.SeriesName)
	if name == "" {
		return nil, false
	}
	feed, ok := c.seriesListing(ctx, seriesSlug(name))
	if !ok || match.NormalizeSeries(feed.name) != match.NormalizeSeries(name) {
		return nil, false
	}
	return feed.entries, true
}

// priorCandidates returns the series members that precede this book, NEAREST FIRST.
//
// The cut is by series position when the book states a parseable one, and otherwise
// by this book's index in the listing (the upstream listing is in series order). A
// book that is in neither has no derivable predecessor.
func priorCandidates(entries []seriesEntry, q SeriesPriorQuery) []string {
	var out []string
	if pos, ok := parseFloatSeq(q.SeriesPos); ok {
		for _, e := range entries {
			if p, ok := parseFloatSeq(e.Pos); ok && p < pos {
				out = append(out, e.ID)
			}
		}
		reverse(out)
		return out
	}
	workID := strings.TrimSpace(q.WorkID)
	if workID == "" {
		return nil
	}
	for _, e := range entries {
		if e.ID == workID {
			reverse(out)
			return out
		}
		out = append(out, e.ID)
	}
	return nil // this book is not in the listing: no cut point, so no predecessor
}

// reverse flips a slice in place (the listing is oldest-first; the walk wants the
// nearest predecessor first).
func reverse(ids []string) {
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
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
		// Chapter 0 is the predecessor's OWN "previously" recap (its prior volume's
		// material), never its story; only chaptered entries describe this book's
		// predecessor.
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
