// The retrieval ladder: the ordered set of search queries a book is looked up
// by when its own title does not retrieve it. This is a pure function over a
// BookIdentity, kept apart from coverage.go's HTTP client and caches because the
// rungs are the interesting part and they are worth reading on their own.
//
// Two guards keep a wider net from becoming a wrong match: a rung's cards are
// scored against the book's own title, never the query (except the folder leaf,
// and only when the tag title does not contradict it - see leafScorable), and an
// accept whose card disagrees with the volume the book claims is rejected and the
// walk continues (see contradictsVolume in coverage.go).
//
// Request budget: an unresolved book costs at most one search per rung (9), and
// usually fewer - rungs that collapse onto each other are de-duplicated, and the
// walk stops at the first rung that matches, so a book whose raw title resolves
// still costs exactly one. That budget is bounded further by coverageWorkers
// (concurrent per-book resolutions during a scan) and by the two caches: the
// searchFeed cache dedupes an identical query across books (every volume in a
// series folder shares its parent-folder rung), and one searchVerd entry covers
// a whole ladder walk, so an unmatched book is not re-walked on the next poll.

package metaops

import (
	"cmp"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// ladderStep is one retrieval attempt: the query sent upstream, and the title
// match.Best scores the returned cards against. The two differ only for a
// path-derived query, where the folder leaf IS the title the tags failed to
// carry - scoring such a card against the shortcode title ("RO07") would reject
// every card the rung retrieved.
type ladderStep struct{ query, matchTitle string }

// minQueryLen is the shortest DERIVED query worth sending, in runes: below it the
// FTS index returns noise rather than a book. The raw-title rung is exempt - it is
// what this daemon searched before the ladder existed, and dropping it would leave
// "It" or "Us" searching nothing at all while still caching a verdict.
const minQueryLen = 3

// searchLadder builds the ordered, de-duplicated retrieval ladder for a book.
// The upstream index matches the words it is given, so a decorated shelf title
// retrieves NOTHING even when the work is present under a clean title - over a
// real 1147-book library that, not scoring, was the dominant cause of the 510
// unresolved books. Each rung is one measured failure shape; rung 1 is the raw
// title, so a book that already resolved still costs exactly one request.
func searchLadder(id BookIdentity) []ladderStep {
	title, author := id.title(), id.author()
	clean := match.CleanTitle(title, id.Series)
	// The folder leaf: the tail behind a shelf prefix ("RO07 - Sandqueen"), else
	// the whole leaf ("Sandqueen"), which is the same evidence without the prefix.
	leaf := strings.TrimSpace(id.FolderName)
	if tail := postSeparator(leaf); tail != "" {
		leaf = tail
	}
	// Whether that leaf may also be what cards are SCORED against. It may not when
	// the tag title says something different: a mis-shelved folder naming another
	// book by the same author would otherwise mint a confident match, and a search
	// verdict flows into books.work_id, which the contributing stage attaches
	// sidecars to - the wrong work there is a spoiler hazard, not a cosmetic slip.
	leafTitle := title
	if leafScorable(title, leaf) {
		leafTitle = leaf
	}
	parent := strings.TrimSpace(id.ParentDir)
	// Rung 7 needs BOTH halves: with no author it would repeat rung 3, and with no
	// title it would be a bare author name - a query for the wrong thing entirely.
	byAuthor := ""
	if clean != "" && author != "" {
		byAuthor = clean + " " + author
	}

	steps := []ladderStep{
		// 1. Today's behaviour, first: the raw shelf title.
		{title, title},
		// 2. Edition/punctuation noise: "Halo: Primordium (Unabridged)" -> "Halo Primordium".
		//    Deliberately NOT CleanTitle: this works around the upstream FTS
		//    TOKENIZER (which does not split on the punctuation), not title
		//    decoration, so it keeps every word rather than dropping any.
		{normalizeQueryPunct(title), title},
		// 3. Series name + "(Book N)" fluff removed by the shared matcher.
		{clean, title},
		// 4. Decorated subtitle: "Supermage : Rise To Omniscience, Book 1" -> "Supermage".
		{preSubtitle(title), title},
		// 5. Shelf prefix: "Artemis Fowl 4 - The Opal Deception" -> "The Opal Deception".
		{postSeparator(title), title},
		// 6. A BARE trailing volume number the matcher leaves behind, since it only
		//    strips a numbered marker that names itself ("Book 5"): "Vorkosigan
		//    Saga 01" -> "Vorkosigan Saga". Derived from the cleaned title so the
		//    keyword vocabulary stays upstream in CleanTitle, in one place.
		{trailingNumber.ReplaceAllString(clean, ""), title},
		// 7. The index carries author names too, which disambiguates a one-word
		//    title that is otherwise buried ("Timeless" -> "Timeless Travis Bagwell").
		{byAuthor, title},
		// 8. The path leaf when the tag title is a shortcode ("RO07 - Sandqueen",
		//    or a plain "Sandqueen" folder). Scored against the leaf when the tag
		//    title does not contradict it, because the leaf is the real title there.
		{leaf, leafTitle},
		// 9. The parent folder, usually the series name ("Rise to Omniscience").
		//    Retrieval only - scoring stays on the book's own title so a series
		//    query cannot resolve to an arbitrary sibling volume. An untagged book
		//    has no such title, and then the folder name is all there is.
		{parent, cmp.Or(title, parent)},
	}

	out := make([]ladderStep, 0, len(steps))
	seen := map[ladderStep]bool{}
	for i, st := range steps {
		st.query = strings.TrimSpace(st.query)
		// The de-duplication key is the WHOLE step, not the query: rung 5 and rung 8
		// can send the same query scored against different titles ("Sandqueen" from
		// the tag title, then from the folder leaf), and dropping the second would
		// silently delete the leaf-scored variant that is the point of the rung.
		if st.query == "" || seen[st] {
			continue
		}
		if i > 0 && utf8.RuneCountInString(st.query) < minQueryLen {
			continue
		}
		seen[st] = true
		out = append(out, st)
	}
	return out
}

// leafScorable reports whether the folder leaf may stand in for the tag title as
// the text cards are scored against. It may when there is no tag title, when the
// title spells the leaf out (the leaf is the same book, minus a shelf prefix), or
// when the title carries no real word at all - a bare shortcode like "RO07" or
// "BDM01" identifies nothing to contradict.
func leafScorable(title, leaf string) bool {
	if title == "" {
		return true
	}
	if leaf == "" {
		return false
	}
	if strings.Contains(strings.ToLower(title), strings.ToLower(leaf)) {
		return true
	}
	return !hasWordToken(title)
}

// hasWordToken reports whether s contains an all-letters token of at least two
// runes - what separates a title that says something from a shelf code, where the
// letters are glued to digits ("RO07") or absent ("01").
func hasWordToken(s string) bool {
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if utf8.RuneCountInString(tok) < 2 {
			continue
		}
		letters := true
		for _, r := range tok {
			if !unicode.IsLetter(r) {
				letters = false
				break
			}
		}
		if letters {
			return true
		}
	}
	return false
}

// volumeClaim is the volume number a book states about ITSELF, used to reject a
// rung's accept that lands on a different volume of the same series. inTitle
// records that the number came from the title, which the rungs rewrite - a
// series-position tag survives every rung untouched.
type volumeClaim struct {
	value   float64
	ok      bool
	inTitle bool
}

// maxTitleVolume bounds a title-derived claim. A trailing number is a volume in
// "Vorkosigan Saga 01" and a temperature in "Fahrenheit 451"; no series runs to
// three digits, so anything larger is part of the title and claims nothing.
const maxTitleVolume = 100

// claimedVolume derives the book's own volume claim: its series-position tag
// first (an explicit column, always trusted), else a bare trailing number in the
// title, else a "Book N"-style marker in it.
func claimedVolume(id BookIdentity) volumeClaim {
	if v, ok := parseFloatSeq(id.SeriesPos); ok {
		return volumeClaim{value: v, ok: true}
	}
	title := id.title()
	for _, m := range [][]string{
		trailingNumber.FindStringSubmatch(title),
		volumeMarker.FindStringSubmatch(title),
	} {
		if m == nil {
			continue
		}
		v, ok := parseFloatSeq(m[1])
		if ok && v <= maxTitleVolume {
			return volumeClaim{value: v, ok: true, inTitle: true}
		}
	}
	return volumeClaim{}
}

// digitRuns counts the runs of digits in s, so a rung whose query dropped one of
// the title's numbers can be told from one that kept them all.
func digitRuns(s string) int {
	runs, in := 0, false
	for _, r := range s {
		switch {
		case unicode.IsDigit(r) && !in:
			runs, in = runs+1, true
		case !unicode.IsDigit(r):
			in = false
		}
	}
	return runs
}

// bracketed matches a parenthetical/bracketed group - "(Unabridged)", "[Dramatized]".
var bracketed = regexp.MustCompile(`[(\[][^)\]]*[)\]]`)

// trailingNumber matches a bare number closing a title, with the separators that
// attach it: "Vorkosigan Saga 01", "Wandering Inn - 7".
var trailingNumber = regexp.MustCompile(`[\s\-_:,]*(\d+(?:\.\d+)?)\s*$`)

// volumeMarker matches a named volume marker anywhere in a title ("Book 5",
// "Vol. 2", "#7") - the number a title states about itself when it does not
// simply end in one.
var volumeMarker = regexp.MustCompile(`(?i)\b(?:book|bk|vol|volume|part|pt|#)\s*\.?\s*(\d+(?:\.\d+)?)\b`)

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
