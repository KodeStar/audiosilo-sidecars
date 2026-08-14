// Package metaops talks to the community metadata API (meta.audiosilo.app): it
// resolves a book's identity to a work and reports which expressive-layer
// sidecars (characters/recaps) that work already has, and it wraps the
// audiosilo-meta folder scanner (pkg/scan) as an async job. It uses ONLY the Go
// stdlib HTTP client, the meta module, and the pure-stdlib fuzzy matcher from
// audiosilo-server's pkg/match - no scraping. Every network path degrades
// gracefully: a down or absent metadata service marks coverage "unavailable" and
// never fails a scan.
package metaops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// coverageTTL bounds how long lookup/work-detail/search-verdict responses are
// cached; searchProxyTTL is the shorter window for the /meta/search proxy feed.
const (
	coverageTTL    = time.Hour
	searchProxyTTL = 5 * time.Minute
)

// httpTimeout bounds a single metadata request.
const httpTimeout = 15 * time.Second

// SearchLimit caps how many work candidates the search endpoints consider (the
// fuzzy-match fallback and the /meta/search proxy both use it). It is the single
// source of truth for the limit - the API's meta-search handler references it too.
const SearchLimit = 20

// cacheCap bounds the number of live entries per ttlCache. A modest cap keeps
// memory flat on a long-lived daemon: the search-verdict cache in particular
// gains an entry per scanned title, so an unbounded map would grow without
// limit. Eviction is best-effort (expired-first, then arbitrary) since the
// cache is a latency optimisation, not a store of record.
const cacheCap = 2048

// Coverage is the per-book coverage verdict merged into scan results and stored
// on a book. It answers the two questions the Library UI asks: is this a known
// work, and which sidecars does it still need.
type Coverage struct {
	// Available is false when the metadata service is disabled (no base URL) or
	// unreachable; the UI then shows "coverage unavailable" and the book stays
	// selectable. Known/HasCharacters/HasRecaps are meaningful only when true.
	Available bool `json:"available"`
	// Known is true when the book resolved to a work in the DB. False means no
	// identity, a lookup miss, or a fuzzy-match miss - the UI shows "unknown".
	Known bool `json:"known"`
	// WorkID is the resolved work id (empty when not Known).
	WorkID string `json:"work_id,omitempty"`
	// MatchedBy records HOW the work was resolved ("asin" | "isbn" | "search" |
	// "manual"), so the UI can convey confidence (an exact identifier vs a fuzzy
	// title match vs a human's choice). Empty when not Known.
	MatchedBy string `json:"matched_by,omitempty"`
	// WorkTitle is the resolved work's title, set for the fuzzy "search" and
	// human "manual" matches so the user can eyeball whether the match is right
	// (an exact asin/isbn match needs no confirmation, so it is omitted there).
	WorkTitle string `json:"work_title,omitempty"`
	// Series is the matched work's primary series membership. It is deliberately
	// carried with the verdict so a successful metadata match can repair missing
	// or weak local tag/path metadata before the book is enqueued.
	Series *SeriesRef `json:"series,omitempty"`
	// HasCharacters / HasRecaps report whether the work already carries that
	// sidecar (so it does not need contributing).
	HasCharacters bool `json:"has_characters"`
	HasRecaps     bool `json:"has_recaps"`
	// Recordings carries the resolved work's audiobook editions for internal
	// contribution provenance. It is deliberately omitted from scan/API JSON.
	Recordings []RecordingRef `json:"-"`
}

// RecordingRef is the public metadata needed to identify the audiobook edition
// behind a sidecar extraction.
type RecordingRef struct {
	ID         string
	Narrators  []string
	RuntimeMin int
	ASINs      []RegionASIN
	ISBNs      []string
}

// RegionASIN identifies one Audible regional listing for a recording.
type RegionASIN struct {
	Region string
	ASIN   string
}

// BookIdentity is the resolution input for CoverageFor: the identifiers plus the
// title/author/series/narrators a fuzzy fallback needs when no identifier
// resolves, and the two path hints the query ladder falls back to.
type BookIdentity struct {
	ASIN    string
	ISBN    string
	Title   string
	Authors []string
	// Narrators are the book's local narrator credits. They are match EVIDENCE,
	// not an identifier: a shelf routinely tags the narrator as the author, and a
	// work card routinely carries the same person on the other side.
	Narrators []string
	Series    string
	SeriesPos string
	// FolderName / ParentDir are the book folder's own name and its parent's.
	// Both are optional (empty = no hint) and are used only to widen retrieval
	// when the tagged title is decorated or a shortcode.
	FolderName string
	ParentDir  string
}

// ttlCache is a small mutex-guarded read-through TTL map shared by the coverage
// caches, so the lock + freshness check lives in one place instead of being
// hand-rolled per cache. Only successful fetches are put; a transport failure
// simply skips the put and is retried next call.
type ttlCache[K comparable, V any] struct {
	mu    sync.Mutex
	now   func() time.Time
	ttl   time.Duration
	items map[K]ttlEntry[V]
}

type ttlEntry[V any] struct {
	at  time.Time
	val V
}

func newTTLCache[K comparable, V any](now func() time.Time, ttl time.Duration) *ttlCache[K, V] {
	return &ttlCache[K, V]{now: now, ttl: ttl, items: map[K]ttlEntry[V]{}}
}

// get returns the cached value for key if present and still fresh.
func (c *ttlCache[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || c.now().Sub(e.at) >= c.ttl {
		var zero V
		return zero, false
	}
	return e.val, true
}

// put stores val for key stamped at now, evicting first if a new key would push
// the map past cacheCap.
func (c *ttlCache[K, V]) put(key K, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; !exists && len(c.items) >= cacheCap {
		c.evictLocked()
	}
	c.items[key] = ttlEntry[V]{at: c.now(), val: val}
}

// evictLocked frees room: it first drops every expired entry, and if the map is
// still at capacity, deletes arbitrary entries until it is under the cap. Caller
// holds c.mu.
func (c *ttlCache[K, V]) evictLocked() {
	now := c.now()
	for k, e := range c.items {
		if now.Sub(e.at) >= c.ttl {
			delete(c.items, k)
		}
	}
	for k := range c.items {
		if len(c.items) < cacheCap {
			break
		}
		delete(c.items, k)
	}
}

// The cached value shapes.
type lookupVal struct {
	workID string
	found  bool
}

type workVal struct {
	title      string
	series     *SeriesRef
	recordings []RecordingRef
	// charNames are the work's character names and aliases, in sidecar order and
	// unfiltered. SeriesGlossary filters them; caching them raw keeps this the
	// single decode of the works/{id} payload.
	charNames []string
	hasChars  bool
	hasRecap  bool
}

// searchVal is a cached fuzzy-match verdict (including a negative one), keyed on
// the normalized title+author+series identity so an unmatched book is not
// re-searched each poll (and two works sharing title+author but not series stay
// distinct).
type searchVal struct {
	workID    string
	workTitle string
	series    *SeriesRef
	matched   bool
}

// SeriesRef is a work's series membership in a search result. ID is the series
// slug, which SeriesGlossary follows to reach the sibling volumes; some upstream
// payloads omit it, so a consumer must tolerate "".
type SeriesRef struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Position string `json:"position"`
}

// WorkSearchResult is one work hit from the /meta/search proxy, flattened to the
// shape the Library UI's manual-match picker consumes (authors as plain names).
type WorkSearchResult struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Authors []string `json:"authors"`
	// Narrators are the card's narrator credits, carried because the fuzzy
	// fallback's author gate consults them (a shelf that tags the narrator as the
	// author still resolves). Additive on the picker's wire shape.
	Narrators []string   `json:"narrators,omitempty"`
	Series    *SeriesRef `json:"series"`
	CoverURL  string     `json:"cover_url"`
}

// Client is the metadata API client with in-memory TTL caches.
type Client struct {
	baseURL string
	http    *http.Client

	lookups    *ttlCache[string, lookupVal]          // key: "asin:<v>" | "isbn:<v>"
	works      *ttlCache[string, workVal]            // key: work id
	searchVerd *ttlCache[string, searchVal]          // key: normalized title|author
	searchFeed *ttlCache[string, []WorkSearchResult] // key: "<limit>:<query>"
	glossaries *ttlCache[string, Glossary]           // key: work id (see glossary.go)
	seriesFeed *ttlCache[string, []string]           // key: series id -> member work ids
}

// ErrDisabled is returned by the search/manual-match paths when the client has no
// base URL (metadata lookup disabled).
var ErrDisabled = errors.New("metadata service disabled")

// ErrWorkNotFound is returned by CoverageForWork when the given work id does not
// resolve to a work in the DB (a clean upstream 404, distinct from a transport
// failure), so a manual match against a stale id reports a clear client error.
var ErrWorkNotFound = errors.New("work not found")

// NewClient returns a metadata client for baseURL. An empty baseURL yields a
// client whose CoverageFor always reports Available=false (metadata disabled).
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		http:       &http.Client{Timeout: httpTimeout},
		lookups:    newTTLCache[string, lookupVal](time.Now, coverageTTL),
		works:      newTTLCache[string, workVal](time.Now, coverageTTL),
		searchVerd: newTTLCache[string, searchVal](time.Now, coverageTTL),
		searchFeed: newTTLCache[string, []WorkSearchResult](time.Now, searchProxyTTL),
		glossaries: newTTLCache[string, Glossary](time.Now, coverageTTL),
		seriesFeed: newTTLCache[string, []string](time.Now, coverageTTL),
	}
}

// Enabled reports whether the client has a metadata base URL to query.
func (c *Client) Enabled() bool { return c.baseURL != "" }

// CoverageFor resolves a book identity to a coverage verdict, trying in
// descending confidence: ASIN lookup, then ISBN lookup, then a fuzzy title
// search. It never returns an error for a network/upstream problem - those
// degrade to Available=false so the caller (the scan) proceeds. An error is
// returned only for a cancelled context.
func (c *Client) CoverageFor(ctx context.Context, id BookIdentity) (Coverage, error) {
	if !c.Enabled() {
		return Coverage{Available: false}, nil
	}
	asin := strings.TrimSpace(id.ASIN)
	isbn := strings.TrimSpace(id.ISBN)

	// 1. ASIN, then 2. ISBN - exact-identifier lookups, highest confidence. A
	// transport failure at either step means the service is unreachable, so the
	// verdict degrades to unavailable across the board rather than a false miss.
	for _, idr := range []struct{ kind, value string }{{"asin", asin}, {"isbn", isbn}} {
		if idr.value == "" {
			continue
		}
		workID, found, ok := c.lookup(ctx, idr.kind, idr.value)
		if err := ctx.Err(); err != nil {
			return Coverage{}, err
		}
		if !ok {
			return Coverage{Available: false}, nil
		}
		if found {
			return c.workCoverage(ctx, workID, idr.kind, ""), nil
		}
	}

	// 3. Fuzzy title search - the fallback that makes coverage useful for the
	// common case (a folder scan with no asin/isbn against a DB seeded from the
	// user's own library). Only attempted when there is a title to match.
	if strings.TrimSpace(id.Title) != "" {
		cov, ok := c.searchMatch(ctx, id)
		if err := ctx.Err(); err != nil {
			return Coverage{}, err
		}
		if !ok {
			return Coverage{Available: false}, nil // upstream unreachable
		}
		return cov, nil
	}

	// Configured and reachable in principle, but nothing to resolve on.
	return Coverage{Available: true, Known: false}, nil
}

// CoverageForWork resolves a work id directly (a human's manual match). Unlike
// the coverage path it distinguishes a clean miss (ErrWorkNotFound) from a
// transport failure, so the API can map a stale id to a 4xx and a down upstream
// to a 502. The verdict carries MatchedBy "manual" and the work title.
func (c *Client) CoverageForWork(ctx context.Context, workID string) (Coverage, error) {
	if !c.Enabled() {
		return Coverage{}, ErrDisabled
	}
	v, found, ok := c.workDetail(ctx, workID)
	if !ok {
		return Coverage{}, fmt.Errorf("work lookup failed for %q", workID)
	}
	if !found {
		return Coverage{}, ErrWorkNotFound
	}
	return Coverage{
		Available: true, Known: true, WorkID: workID,
		MatchedBy: "manual", WorkTitle: v.title, Series: cloneSeriesRef(v.series),
		HasCharacters: v.hasChars, HasRecaps: v.hasRecap,
		Recordings: cloneRecordingRefs(v.recordings),
	}, nil
}

// SearchWorks proxies a free-text query to the metadata search endpoint, keeping
// only work hits and flattening them to the picker DTO. It returns ErrDisabled
// when unconfigured and a transport error otherwise. Results are cached briefly.
func (c *Client) SearchWorks(ctx context.Context, query string, limit int) ([]WorkSearchResult, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	res, ok := c.fetchWorkSearch(ctx, query, limit)
	if !ok {
		return nil, fmt.Errorf("metadata search failed for %q", query)
	}
	return res, nil
}

// workCoverage builds a Known verdict for workID, folding in the work's sidecar
// presence (best-effort - a failed detail probe leaves has_* false). workTitle
// overrides the resolved title (the search path supplies the matched card's
// title); an empty workTitle leaves the field unset for asin/isbn matches.
func (c *Client) workCoverage(ctx context.Context, workID, matchedBy, workTitle string) Coverage {
	cov := Coverage{
		Available: true, Known: true, WorkID: workID,
		MatchedBy: matchedBy, WorkTitle: workTitle,
	}
	if v, found, ok := c.workDetail(ctx, workID); ok && found {
		cov.HasCharacters = v.hasChars
		cov.HasRecaps = v.hasRecap
		cov.Series = cloneSeriesRef(v.series)
		cov.Recordings = cloneRecordingRefs(v.recordings)
	}
	return cov
}

func cloneSeriesRef(s *SeriesRef) *SeriesRef {
	if s == nil {
		return nil
	}
	copy := *s
	return &copy
}

func cloneRecordingRefs(in []RecordingRef) []RecordingRef {
	out := make([]RecordingRef, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Narrators = append([]string(nil), in[i].Narrators...)
		out[i].ASINs = append([]RegionASIN(nil), in[i].ASINs...)
		out[i].ISBNs = append([]string(nil), in[i].ISBNs...)
	}
	return out
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

// lookup resolves an identifier to a work id, cached per identifier. ok=false
// means the upstream was unreachable; found=false means a clean miss (no such
// work). kind is "asin" or "isbn".
func (c *Client) lookup(ctx context.Context, kind, value string) (workID string, found, ok bool) {
	key := kind + ":" + value
	if v, hit := c.lookups.get(key); hit {
		return v.workID, v.found, true
	}
	q := url.Values{}
	q.Set(kind, value)
	var res struct {
		Work *struct {
			ID string `json:"id"`
		} `json:"work"`
	}
	f, okc := c.getJSON(ctx, "/api/v1/lookup?"+q.Encode(), &res)
	if !okc {
		return "", false, false
	}
	v := lookupVal{}
	if f && res.Work != nil {
		v.workID = res.Work.ID
		v.found = true
	}
	c.lookups.put(key, v)
	return v.workID, v.found, true
}

// workDetail fetches a work's title + sidecar presence, cached per work. Sidecar
// keys are omitempty on the wire, so a present, non-empty array means the work
// carries that sidecar. found=false is a clean 404; ok=false is a transport
// failure (the two are distinguished so a manual match can 404 vs 502).
func (c *Client) workDetail(ctx context.Context, workID string) (v workVal, found, ok bool) {
	if cached, hit := c.works.get(workID); hit {
		return cached, true, true
	}
	var res struct {
		Title  string      `json:"title"`
		Series []SeriesRef `json:"series"`
		// Characters carries the names too, not just presence: SeriesGlossary needs
		// them, and decoding them here keeps ONE mirror of the works/{id} payload
		// and one cache entry per work (a sibling of the book being processed is
		// usually already warm from the coverage path).
		Characters []struct {
			Name    string   `json:"name"`
			Aliases []string `json:"aliases"`
		} `json:"characters"`
		Recaps     []json.RawMessage `json:"recaps"`
		Recordings []struct {
			ID        string `json:"id"`
			Narrators []struct {
				Name string `json:"name"`
			} `json:"narrators"`
			RuntimeMin int          `json:"runtime_min"`
			ASINs      []RegionASIN `json:"asin"`
			ISBNs      []string     `json:"isbn"`
		} `json:"recordings"`
	}
	f, okc := c.getJSON(ctx, "/api/v1/works/"+url.PathEscape(workID), &res)
	if !okc {
		return workVal{}, false, false
	}
	if !f {
		return workVal{}, false, true
	}
	v = workVal{title: res.Title, hasChars: len(res.Characters) > 0, hasRecap: len(res.Recaps) > 0}
	if len(res.Series) > 0 {
		v.series = cloneSeriesRef(&res.Series[0])
	}
	for _, ch := range res.Characters {
		v.charNames = append(v.charNames, ch.Name)
		v.charNames = append(v.charNames, ch.Aliases...)
	}
	for _, rec := range res.Recordings {
		r := RecordingRef{ID: rec.ID, RuntimeMin: rec.RuntimeMin, ASINs: rec.ASINs, ISBNs: rec.ISBNs}
		for _, narrator := range rec.Narrators {
			r.Narrators = append(r.Narrators, narrator.Name)
		}
		v.recordings = append(v.recordings, r)
	}
	c.works.put(workID, v)
	return v, true, true
}

// searchMatch runs (and caches) the fuzzy-match fallback: it walks the retrieval
// ladder (see searchLadder), scores each query's work candidates with match.Best
// - plus a narrator-evidence pass - and accepts the FIRST query that yields an
// acceptance. ok=false is a transport failure; otherwise the returned Coverage
// is either a "search" match or a clean unknown.
//
// The ladder is retrieval only: matching stays match.Best's decision, so a wider
// query can surface a work but never lower the bar for accepting it.
func (c *Client) searchMatch(ctx context.Context, id BookIdentity) (Coverage, bool) {
	title := strings.TrimSpace(id.Title)
	author := firstNonEmpty(id.Authors)
	// The verdict key includes the series identity (name + position), not just
	// title+author: match.Best weighs the series/sequence, so two distinct works
	// that share a title and author but sit in different series (or at different
	// positions) can resolve to different works - keying on title+author alone
	// would let one inherit the other's cached verdict. The path hints and
	// narrators join it for the same reason: both steer the ladder, so two books
	// agreeing on title+author+series can still reach different verdicts.
	key := strings.Join([]string{
		match.Normalize(title), match.Normalize(author),
		match.NormalizeSeries(id.Series), strings.TrimSpace(id.SeriesPos),
		match.Normalize(id.FolderName), match.Normalize(id.ParentDir),
		match.Normalize(strings.Join(id.Narrators, " ")),
	}, "|")
	if v, hit := c.searchVerd.get(key); hit {
		return c.searchCoverage(ctx, v), true
	}

	seq, hasSeq := parseFloatSeq(id.SeriesPos)
	v := searchVal{}
	for _, step := range searchLadder(id) {
		cards, ok := c.fetchWorkSearch(ctx, step.query, SearchLimit)
		if !ok {
			// A transport failure anywhere in the ladder degrades the whole verdict to
			// unavailable, exactly as the single-query version did: a partial ladder
			// cannot tell "this book is unknown" from "the service went away".
			return Coverage{}, false
		}
		if len(cards) == 0 {
			continue
		}
		books := cardBooks(cards)
		q := match.Query{
			Title: step.matchTitle, TitleShort: step.matchTitle, Author: author,
			Series: id.Series, Sequence: seq, HasSequence: hasSeq,
		}
		idx, matched := match.Best(books, q)
		if !matched {
			idx, matched = bestByNarrator(cards, books, q, id.Narrators)
		}
		if matched {
			v = searchVal{
				matched: true, workID: cards[idx].ID, workTitle: cards[idx].Title,
				series: cloneSeriesRef(cards[idx].Series),
			}
			break
		}
	}
	// One verdict per identity covers the WHOLE ladder: an unmatched book must not
	// re-walk every rung on the next poll.
	c.searchVerd.put(key, v)
	return c.searchCoverage(ctx, v), true
}

// searchCoverage turns a (possibly negative) cached search verdict into the
// coverage the caller sees, falling back to the card's series when the work
// detail carries none.
func (c *Client) searchCoverage(ctx context.Context, v searchVal) Coverage {
	if !v.matched {
		return Coverage{Available: true, Known: false}
	}
	cov := c.workCoverage(ctx, v.workID, "search", v.workTitle)
	if cov.Series == nil {
		cov.Series = cloneSeriesRef(v.series)
	}
	return cov
}

// cardBooks maps search cards onto match.Book candidates (index-aligned).
func cardBooks(cards []WorkSearchResult) []match.Book {
	books := make([]match.Book, len(cards))
	for i, card := range cards {
		b := match.Book{Title: card.Title, Author: firstNonEmpty(card.Authors)}
		if card.Series != nil {
			b.Series = card.Series.Name
			idx, _ := parseFloatSeq(card.Series.Position)
			b.SeriesIndex = idx
		}
		books[i] = b
	}
	return books
}

// ladderStep is one retrieval attempt: the query sent upstream, and the title
// match.Best scores the returned cards against. The two differ only for a
// path-derived query, where the folder leaf IS the title the tags failed to
// carry - scoring such a card against the shortcode title ("RO07") would reject
// every card the rung retrieved.
type ladderStep struct{ query, matchTitle string }

// minQueryLen is the shortest query worth sending: below it the FTS index
// returns noise rather than a book.
const minQueryLen = 3

// searchLadder builds the ordered, de-duplicated retrieval ladder for a book.
// The upstream index matches the words it is given, so a decorated shelf title
// retrieves NOTHING even when the work is present under a clean title - over a
// real 1147-book library that, not scoring, was the dominant cause of the 510
// unresolved books. Each rung is one measured failure shape; rung 1 is the raw
// title, so a book that already resolved still costs exactly one request.
func searchLadder(id BookIdentity) []ladderStep {
	title := strings.TrimSpace(id.Title)
	author := firstNonEmpty(id.Authors)
	clean := match.CleanTitle(title, id.Series)
	folderTail := postSeparator(id.FolderName)

	steps := []ladderStep{
		// 1. Today's behaviour, first: the raw shelf title.
		{title, title},
		// 2. Edition/punctuation noise: "Halo: Primordium (Unabridged)" -> "Halo Primordium".
		{normalizeQueryPunct(title), title},
		// 3. Series name + "(Book N)" fluff removed by the shared matcher.
		{clean, title},
		// 4. Decorated subtitle: "Supermage : Rise To Omniscience, Book 1" -> "Supermage".
		{preSubtitle(title), title},
		// 5. Shelf prefix: "Artemis Fowl 4 - The Opal Deception" -> "The Opal Deception".
		{postSeparator(title), title},
		// 6. Trailing volume marker: "Vorkosigan Saga 01" -> "Vorkosigan Saga".
		{stripTrailingVolume(title), title},
		// 7. The index carries author names too, which disambiguates a one-word
		//    title that is otherwise buried ("Timeless" -> "Timeless Travis Bagwell").
		{joinQuery(clean, author), title},
		// 8. The path leaf when the tag title is a shortcode ("RO07 - Sandqueen").
		//    Scored against the leaf, because the leaf is the real title here.
		{folderTail, folderTail},
		// 9. The parent folder, usually the series name ("Rise to Omniscience").
		//    Retrieval only - scoring stays on the book's own title so a series
		//    query cannot resolve to an arbitrary sibling volume.
		{strings.TrimSpace(id.ParentDir), title},
	}

	out := make([]ladderStep, 0, len(steps))
	seen := map[string]bool{}
	for i, st := range steps {
		q := strings.TrimSpace(st.query)
		if q == "" || seen[q] {
			continue
		}
		// The length floor exists to stop DERIVED fragments sending noise queries; it
		// must not ban a real short title. Rung 1 IS the title, so it is exempt:
		// otherwise "It" filtered every rung away and searchMatch cached a negative
		// verdict having never asked upstream at all, where the pre-ladder code always
		// queried the raw title.
		if i > 0 && len(q) < minQueryLen {
			continue
		}
		seen[q] = true
		st.query = q
		if strings.TrimSpace(st.matchTitle) == "" {
			st.matchTitle = q
		}
		out = append(out, st)
	}
	return out
}

// bracketed matches a parenthetical/bracketed group - "(Unabridged)", "[Dramatized]".
var bracketed = regexp.MustCompile(`[(\[][^)\]]*[)\]]`)

// trailingVolume matches a volume marker at the very end of a title, with or
// without its keyword: "Vorkosigan Saga 01", "Cradle Book 3", "Wandering Inn #7".
var trailingVolume = regexp.MustCompile(`(?i)[\s\-_:,]*(?:book|bk|vol|volume|part|pt|#)?[\s.]*\d+(?:\.\d+)?$`)

// normalizeQueryPunct drops bracketed edition noise and turns title punctuation
// into spaces, leaving the bare words the index is tokenized on.
func normalizeQueryPunct(s string) string {
	s = bracketed.ReplaceAllString(s, " ")
	s = strings.Map(func(r rune) rune {
		if r == ':' || r == ',' || r == ';' {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// preSubtitle returns the segment before the first ':' or ',' (the work's own
// title, ahead of the series/edition decoration). Empty when there is none.
func preSubtitle(s string) string {
	i := strings.IndexAny(s, ":,")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(s[:i])
}

// postSeparator returns the tail after the LAST " - " or ": " (the work's own
// title, behind a shelf/series prefix). Empty when there is none.
func postSeparator(s string) string {
	sep, at := -1, 0
	for _, d := range []string{" - ", ": "} {
		if i := strings.LastIndex(s, d); i > sep {
			sep, at = i, i+len(d)
		}
	}
	if sep < 0 {
		return ""
	}
	return strings.TrimSpace(s[at:])
}

// stripTrailingVolume removes a trailing volume marker from a title.
func stripTrailingVolume(s string) string {
	return strings.TrimSpace(trailingVolume.ReplaceAllString(s, ""))
}

// joinQuery joins two query fragments, yielding "" unless both are present.
func joinQuery(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return ""
	}
	return a + " " + b
}

// bestByNarrator is the narrator-evidence pass run after match.Best declines a
// query's cards. Shelves routinely credit the NARRATOR as the author ("How to
// Break a Dragon's Heart" tagged "David Tennant"), and match.Best's author gate
// then rejects the correct card outright. When a card's narrators name the
// query's author - or the book's own narrators name the card's author - the two
// records are talking about the same recording, so the card's author is
// substituted into the query and match.Best is re-run over just those cards.
// Only what match.Best accepts is accepted: title/series scoring is untouched.
func bestByNarrator(cards []WorkSearchResult, books []match.Book, q match.Query, narrators []string) (int, bool) {
	var idxs []int          // linked card indices, into cards/books
	var subset []match.Book // the linked candidates, index-aligned with idxs
	var authors []string    // their distinct substituted authors, in card order
	for i, card := range cards {
		author := firstNonEmpty(card.Authors)
		if author == "" || !linkedByNarrator(card, q.Author, narrators) {
			continue
		}
		idxs = append(idxs, i)
		subset = append(subset, books[i])
		// Deduped at insert (never by iterating a map) so the pass order below stays
		// deterministic: two authors can both match, and the first card wins.
		if !slices.Contains(authors, author) {
			authors = append(authors, author)
		}
	}
	if len(idxs) == 0 {
		return -1, false
	}
	// One pass per DISTINCT substituted author: match.Best takes a single query
	// author, so linked cards by different authors need their own pass.
	for _, author := range authors {
		sub := q
		sub.Author = author
		if j, ok := match.Best(subset, sub); ok {
			return idxs[j], true
		}
	}
	return -1, false
}

// linkedByNarrator reports narrator evidence tying a card to the book: the query
// author appears among the card's narrators, or a local narrator appears among
// the card's authors.
func linkedByNarrator(card WorkSearchResult, queryAuthor string, narrators []string) bool {
	for _, n := range card.Narrators {
		if namesMatch(queryAuthor, n) {
			return true
		}
	}
	for _, local := range narrators {
		for _, a := range card.Authors {
			if namesMatch(local, a) {
				return true
			}
		}
	}
	return false
}

// namesMatch is pkg/match's tolerant person-name rule (normalize, then equality
// or containment; an empty side never matches), mirrored here because the
// matcher keeps it unexported.
func namesMatch(a, b string) bool {
	na, nb := match.Normalize(a), match.Normalize(b)
	if na == "" || nb == "" {
		return false
	}
	return na == nb || strings.Contains(na, nb) || strings.Contains(nb, na)
}

// fetchWorkSearch runs the search endpoint and returns work-kind hits flattened
// to WorkSearchResult, cached briefly per (limit, query). ok=false is a transport
// failure; an empty result is (nil-or-empty, true).
func (c *Client) fetchWorkSearch(ctx context.Context, query string, limit int) ([]WorkSearchResult, bool) {
	if limit <= 0 || limit > SearchLimit {
		limit = SearchLimit
	}
	key := strconv.Itoa(limit) + ":" + query
	if v, hit := c.searchFeed.get(key); hit {
		return v, true
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", strconv.Itoa(limit))
	var res struct {
		Results []struct {
			Kind    string `json:"kind"`
			ID      string `json:"id"`
			Title   string `json:"title"`
			Authors []struct {
				Name string `json:"name"`
			} `json:"authors"`
			Narrators []struct {
				Name string `json:"name"`
			} `json:"narrators"`
			Series *struct {
				Name     string `json:"name"`
				Position string `json:"position"`
			} `json:"series"`
			CoverURL *string `json:"cover_url"`
		} `json:"results"`
	}
	found, ok := c.getJSON(ctx, "/api/v1/search?"+q.Encode(), &res)
	if !ok {
		return nil, false
	}
	out := []WorkSearchResult{}
	if found {
		for _, r := range res.Results {
			if r.Kind != "work" {
				continue
			}
			w := WorkSearchResult{ID: r.ID, Title: r.Title, Authors: []string{}}
			for _, a := range r.Authors {
				if a.Name != "" {
					w.Authors = append(w.Authors, a.Name)
				}
			}
			for _, n := range r.Narrators {
				if n.Name != "" {
					w.Narrators = append(w.Narrators, n.Name)
				}
			}
			if r.Series != nil {
				w.Series = &SeriesRef{Name: r.Series.Name, Position: r.Series.Position}
			}
			if r.CoverURL != nil {
				w.CoverURL = *r.CoverURL
			}
			out = append(out, w)
		}
	}
	c.searchFeed.put(key, out)
	return out, true
}

// firstNonEmpty returns the first non-blank string in ss (used to pick the
// primary author for matching).
func firstNonEmpty(ss []string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// parseFloatSeq parses a series position to (value, ok). An omnibus range or a
// non-numeric position yields ok=false so a fuzzy match never asserts a false
// sequence.
func parseFloatSeq(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "-") {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
