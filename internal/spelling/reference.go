package spelling

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ReferenceMatchesFile is the deterministic report handed to the spelling_research
// agent alongside spelling_candidates.json: the transcript forms that look like
// misspellings of a name a REFERENCE source spells differently.
//
// It exists because the agent's own signal is intra-transcript disagreement, and a
// uniformly misheard name produces none. "Torrin" appearing 369 times and never
// once as "Toren" reads as settled - there is nothing to adjudicate - so the name
// survives every variant-and-cluster pass. Comparing against an outside vocabulary
// is the only way that class of error becomes visible at all.
const ReferenceMatchesFile = "spelling_reference_matches.json"

// Reference-match bounds. The report is staged into an agent prompt, so it stays
// small; a drop is never silent (ReferenceMatches.Truncated).
const (
	maxReferenceMatches = 200
	// minMatchRunes is the shortest form worth comparing. Short forms are dominated
	// by unrelated collisions at any edit distance ("Ana"/"Ann", "Rag"/"Rat").
	minMatchRunes = 4
	// maxMatchDistance is the absolute edit-distance ceiling for a proposal.
	maxMatchDistance = 2
	// matchLenDivisor scales the allowed distance with length: distance must also be
	// <= len/matchLenDivisor, so a short form gets one edit and a long one gets two.
	// Calibrated against the six real Wandering Inn misspellings (Torrin/Toren d2,
	// Floss/Flos d1, Terriarch/Teriarch d1, Goddard/Godart d2, prognogator/
	// Prognugator d1, Valsaif/Valceif d2) - all pass.
	matchLenDivisor = 3
)

// Authority ranks a reference source. It is the precedence rule the whole report
// turns on.
//
// Scope: this ranking is advisory, addressed to the agent through the report and
// the prompt. Check's gate 3 still treats every cited reference_files entry as
// equally attesting, so a carryover-only attestation can still pass the gate -
// deliberately, since Check is a contract-frozen golden-tested port (the same
// reasoning that keeps DeadRules out of it rather than making it a fifth gate).
type Authority string

const (
	// AuthorityVerified is an outside, non-transcript source: the community
	// metadata database's canonical names, or the publisher's own chapter titles.
	// It OUTRANKS the transcript however many times the transcript disagrees.
	AuthorityVerified Authority = "verified"
	// AuthorityCarryover is the previous volume's own output (its ledger and
	// corrected text). It is evidence of consistency, NOT of correctness: a
	// previous book's mistake is carried in it verbatim, which is exactly how one
	// misheard name propagates across a whole series.
	AuthorityCarryover Authority = "carryover"
)

// ReferenceSource is one vocabulary the candidates are compared against.
type ReferenceSource struct {
	// Name is the citation name, as a reference_files entry would spell it
	// ("marker_titles.txt", "spelling-refs/series-glossary.txt").
	Name string
	// Authority ranks this source against the others.
	Authority Authority
	// Names is the source's vocabulary.
	Names []string
}

// ReferenceMatch is one proposal: a transcript form that no reference source knows,
// which is one or two edits from a name a reference source does spell.
type ReferenceMatch struct {
	// Form is the transcript form, exactly as spelling_candidates.json lists it (so
	// a rule pattern can copy it verbatim).
	Form string `json:"form"`
	// Count is how many times Form occurs in the transcript.
	Count int `json:"count"`
	// Chapters are the chapters Form appears in (capped by the candidate report).
	Chapters []int `json:"chapters,omitempty"`
	// Reference is the reference source's spelling.
	Reference string `json:"reference"`
	// Source names the reference file the spelling came from.
	Source string `json:"source"`
	// Authority is Source's rank - "verified" outranks the transcript, "carryover"
	// does not.
	Authority Authority `json:"authority"`
	// Distance is the edit distance between Form and Reference.
	Distance int `json:"distance"`
}

// ReferenceMatches is the ReferenceMatchesFile contract.
type ReferenceMatches struct {
	// Sources lists the reference vocabularies consulted, with their authority.
	Sources []ReferenceSourceInfo `json:"sources"`
	// Matches is the proposal list, verified sources first, then by count desc.
	Matches []ReferenceMatch `json:"matches"`
	// Truncated counts proposals dropped by maxReferenceMatches (0 = none).
	Truncated int `json:"truncated,omitempty"`
}

// ReferenceSourceInfo records a consulted source in the report itself, so the agent
// (and a human reading the staged dir later) sees what was and was not available.
type ReferenceSourceInfo struct {
	Name      string    `json:"name"`
	Authority Authority `json:"authority"`
	Names     int       `json:"names"`
}

// Empty reports whether the report proposes nothing.
func (r *ReferenceMatches) Empty() bool { return r == nil || len(r.Matches) == 0 }

// BuildReferenceMatches compares every candidate form against the reference
// vocabularies and returns the near-miss proposals.
//
// Two rules keep the output precise:
//
//  1. A candidate whose exact form is in a VERIFIED vocabulary is skipped outright -
//     an outside source already spells it that way, so there is nothing to propose.
//     This is what keeps two genuinely distinct near-identical names apart: "Tersa"
//     and "Terra" are one edit apart, but both are canonical names, so neither is
//     proposed as a correction of the other. A carryover match deliberately does NOT
//     settle a form (see `known` below).
//  2. A proposal must share its first letter with the reference and satisfy both
//     distance bounds.
//
// The result is advisory. It never rewrites anything: the agent still decides, and
// the do-not-merge rule still governs two distinct characters.
func BuildReferenceMatches(c *Candidates, sources []ReferenceSource) *ReferenceMatches {
	out := &ReferenceMatches{}
	if c == nil {
		return out
	}

	// known holds the spellings that settle a form, case-folded. Only VERIFIED
	// sources contribute.
	//
	// A carryover source must never confer known status, however exactly it matches.
	// The previous volume spelling a name "Torrin" is not evidence that "Torrin" is
	// right - it is the same ASR error, one book earlier. Letting it into this set
	// would make the propagated mistake immunise itself: the form would look settled
	// and the glossary's correction would never be proposed, which is precisely the
	// mechanism that carried one misheard name across a whole series.
	known := map[string]bool{}
	var refs []refName
	addRef := func(name, source string, authority Authority) {
		if len([]rune(name)) < minMatchRunes {
			return
		}
		folded := foldForm(name)
		if authority == AuthorityVerified {
			known[folded] = true
		}
		first, _ := utf8.DecodeRuneInString(folded)
		refs = append(refs, refName{name: name, source: source, authority: authority, folded: folded, first: first})
	}
	for _, src := range sources {
		n := 0
		for _, name := range src.Names {
			name = strings.TrimSpace(name)
			if len([]rune(name)) < minMatchRunes {
				continue
			}
			addRef(name, src.Name, src.Authority)
			// A multi-word name is also indexed by its PARTS. Both vocabularies emit
			// only whole names ("Laken Godart"), and the transcript names a character
			// by first name alone constantly - so without this a correctly spelled
			// "Laken" is not settled by anything and can be proposed as a misspelling
			// of some other name entirely, while a misheard "Goddard" finds no match
			// at all because the whole-name comparison never gets past the first
			// letter. Ordinary words are excluded so a title-ish part cannot enter the
			// vocabulary as if it were a name.
			if parts := strings.Fields(name); len(parts) > 1 {
				for _, part := range parts {
					lower := strings.ToLower(part)
					if commonWords[lower] || markerBoilerplate[lower] {
						continue
					}
					addRef(part, src.Name, src.Authority)
				}
			}
			n++
		}
		out.Sources = append(out.Sources, ReferenceSourceInfo{Name: src.Name, Authority: src.Authority, Names: n})
	}
	if len(refs) == 0 {
		return out
	}

	// The transcript's own vocabulary, used only to tell a genuine plural from a
	// misspelling that happens to end in s (see knownForm).
	inTranscript := make(map[string]bool, len(c.Candidates))
	for _, cand := range c.Candidates {
		inTranscript[foldForm(strings.TrimSpace(cand.Form))] = true
	}

	for _, cand := range c.Candidates {
		form := strings.TrimSpace(cand.Form)
		if len([]rune(form)) < minMatchRunes || !properNounShaped(form) {
			continue
		}
		folded := foldForm(form)
		if knownForm(known, inTranscript, folded) {
			continue // rule 1: an outside source already spells it this way
		}
		best, ok := bestRef(folded, refs)
		if !ok {
			continue
		}
		out.Matches = append(out.Matches, ReferenceMatch{
			Form:      cand.Form,
			Count:     cand.Count,
			Chapters:  cand.Chapters,
			Reference: best.name,
			Source:    best.source,
			Authority: best.authority,
			Distance:  best.distance,
		})
	}

	sort.SliceStable(out.Matches, func(i, j int) bool {
		a, b := out.Matches[i], out.Matches[j]
		if (a.Authority == AuthorityVerified) != (b.Authority == AuthorityVerified) {
			return a.Authority == AuthorityVerified
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Form < b.Form
	})
	if len(out.Matches) > maxReferenceMatches {
		out.Truncated = len(out.Matches) - maxReferenceMatches
		out.Matches = out.Matches[:maxReferenceMatches]
	}
	return out
}

// refName is one reference spelling with its provenance. folded is precomputed:
// bestRef compares every candidate against every reference, so folding on demand
// there re-folds the same name once per candidate (measured at ~98% of the pass's
// runtime and 96% of its allocations on a realistic book).
type refName struct {
	name      string
	source    string
	authority Authority
	folded    string
	// first is the folded form's first RUNE. bestRef gates on it, and comparing
	// bytes there both wrongly admits two different non-ASCII letters that share a
	// lead byte and wrongly rejects the commonest ASR rendering of an accented name
	// ("Emile" against "Émile").
	first rune
}

// bestMatch is the winning reference for one candidate.
type bestMatch struct {
	refName
	distance int
}

// bestRef picks the closest reference name for a folded candidate form, preferring
// a verified source over a carryover one at equal distance so the precedence rule
// survives into the proposal itself.
func bestRef(folded string, refs []refName) (bestMatch, bool) {
	var best bestMatch
	found := false
	limit := len([]rune(folded)) / matchLenDivisor
	if limit > maxMatchDistance {
		limit = maxMatchDistance
	}
	if limit < 1 {
		return best, false
	}
	candFirst, _ := utf8.DecodeRuneInString(folded)
	for _, r := range refs {
		rf := r.folded
		if rf == "" || r.first != candFirst {
			continue // cheap precision gate: a correction keeps the first letter
		}
		d := levenshtein(folded, rf, limit)
		// d == 0 means this source spells the form exactly as the transcript does,
		// so there is nothing to propose. It is reachable only for a carryover
		// source (a verified one would have marked the form known and skipped it).
		if d <= 0 || d > limit {
			continue
		}
		better := !found || d < best.distance ||
			(d == best.distance && r.authority == AuthorityVerified && best.authority != AuthorityVerified)
		if better {
			best = bestMatch{refName: r, distance: d}
			found = true
		}
	}
	return best, found
}

// properNounShaped reports whether a candidate is capitalized like the names in the
// reference vocabularies.
//
// Every reference name is a proper noun, and rewriting an ordinary lowercase word
// into one is a different and far riskier operation than fixing a misheard name -
// on a real book this gate is what separates "Olesum -> Olesm" from "glared ->
// Garen", "mass -> Mars" and "grunted -> Grunter", which the extractor lists as
// rare-lowercase candidates and which sit one or two edits from a real character.
// It uses the package's own isCapitalized rather than testing the first rune, so
// the d'Aston shape (lowercase first letter, capital after an internal apostrophe)
// still counts as a name - that token shape is one ExtractCandidates deliberately
// emits, and a first-rune test would drop exactly those candidates before they were
// ever compared.
func properNounShaped(form string) bool {
	if !isCapitalized(form) {
		return false
	}
	// A contraction is capitalized at the start of a sentence and reads as the
	// d'Aston shape to isCapitalized, but "He'd" is not a name and sits one edit
	// from "Head". Judge it by the word before the apostrophe.
	if i := strings.Index(form, "'"); i > 0 && commonWords[strings.ToLower(form[:i])] {
		return false
	}
	return true
}

// knownForm reports whether an already-folded candidate is settled by the verified
// vocabulary, either exactly or as an inflection of a name in it.
//
// Inflections matter because the candidate extractor lists each surface form
// separately: "Magnolia's" and "Godarts" arrive as their own candidates, distinct
// from "Magnolia" and "Godart". Each is one or two edits from the bare name, so
// without this they read as misspellings OF it, and a rule written from such a
// proposal would strip the possessive or the plural throughout the book.
//
// The two inflections are NOT treated alike:
//
//   - A possessive is unambiguous. Apostrophe-s is never part of a name's spelling,
//     so "magnolia's" over a known "magnolia" is always the possessive.
//   - A plural is ambiguous, and dangerously so: "Floss" is exactly "Flos" plus an
//     s, but it is the ASR's misspelling of it, not a plural of it. Suppressing on
//     spelling alone would silence the single most common error in this corpus. So a
//     plural reading additionally requires EVIDENCE - the singular has to actually
//     occur in the transcript. "Godart" appears alongside "Godarts", so that is a
//     plural; "Flos" never appears at all, so "Floss" is not one.
func knownForm(known, inTranscript map[string]bool, folded string) bool {
	if known[folded] {
		return true
	}
	if base, ok := strings.CutSuffix(folded, "'s"); ok && known[base] {
		return true
	}
	// A known name followed by a stray short token is that name, not a misspelling
	// of it. The extractor emits phrase candidates, so "Antinium I" and "Drake I"
	// arrive as their own forms and sit two edits from the bare name.
	if i := strings.LastIndex(folded, " "); i > 0 {
		if head, tail := folded[:i], folded[i+1:]; len([]rune(tail)) < minMatchRunes && known[head] {
			return true
		}
	}
	for _, suffix := range []string{"es", "s"} {
		if base, ok := strings.CutSuffix(folded, suffix); ok && known[base] && inTranscript[base] {
			return true
		}
	}
	return false
}

// foldForm normalizes a form for comparison: apostrophes unified, case folded, and
// runs of whitespace collapsed to one space.
func foldForm(s string) string {
	s = normalizeApostrophes(s)
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// levenshtein returns the edit distance between a and b, or -1 as soon as every
// value in a row exceeds limit (the caller only cares about small distances, so the
// early exit keeps an all-pairs comparison cheap).
func levenshtein(a, b string, limit int) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	if d := len(ar) - len(br); max(d, -d) > limit {
		return -1
	}
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
			if cur[j] < rowMin {
				rowMin = cur[j]
			}
		}
		if rowMin > limit {
			return -1
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

// markerBoilerplate is the structural vocabulary of an audiobook marker table.
// These words are capitalized and are not ordinary English prose, so the shared
// common-word list does not catch them - but they name no character. Admitting one
// is worse than missing a name: a bogus vocabulary entry both invites nonsense
// proposals AND suppresses real ones (a candidate matching a "known" spelling is
// skipped outright).
var markerBoilerplate = map[string]bool{
	"about": true, "acknowledgements": true, "acknowledgments": true, "afterword": true,
	"appendix": true, "audible": true, "audiobook": true, "author": true, "bonus": true,
	"cast": true, "chapter": true, "chapters": true, "contents": true, "copyright": true,
	"credits": true, "dedication": true, "end": true, "ending": true, "epigraph": true,
	"epilogue": true, "excerpt": true, "finale": true, "foreword": true, "glossary": true,
	"interlude": true, "intermission": true, "introduction": true, "map": true,
	"narrator": true, "opening": true, "part": true, "preface": true, "prelude": true,
	"preview": true, "prologue": true, "sample": true, "section": true, "story": true,
	"title": true, "volume": true,
}

// NamesFromTitles extracts the capitalized name-like forms from a plain-text file
// of chapter titles: single capitalized tokens and runs of them ("Toren", "Laken
// Godart"). Ordinary title words are dropped via the shared common-word list,
// structural marker words via markerBoilerplate, and a bare-number table ("001",
// "002" - which is what a real Wandering Inn volume ships) yields nothing.
func NamesFromTitles(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(normalizeApostrophes(text), "\n") {
		// The whole line is mined, label and all. Slicing at a colon looks tidier but
		// silently discards names: "Toren: The Bridge" (the POV-name-as-title
		// convention) yields nothing, and "Chapter 12: Toren: A Reckoning" drops the
		// character while admitting a junk noun in its place. The token filters below
		// already remove the label - "Chapter" is marker boilerplate and a bare
		// numeral is not a capitalized token - so the slice bought nothing.
		// Tokenize with the package's own tokenizer so both sides of the comparison
		// are split the same way. It also matters that isCapitalized accepts the
		// d'Aston shape (lowercase first letter, capital after an internal
		// apostrophe), which a plain unicode.IsUpper on the first rune would reject.
		toks := tokenize(line)
		start := -1
		flush := func(end int) {
			if start < 0 {
				return
			}
			name := phraseForm(toks, start, end)
			start = -1
			lower := strings.ToLower(name)
			if len([]rune(name)) < minMatchRunes || commonWords[lower] || markerBoilerplate[lower] {
				return
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		for i, tk := range toks {
			lower := strings.ToLower(tk.form)
			if !isCapitalized(tk.form) || commonWords[lower] || markerBoilerplate[lower] {
				flush(i)
				continue
			}
			// A run may only span tokens separated by plain spaces, so a comma or
			// quote ends the name rather than fusing two of them.
			if start >= 0 && !tk.joinable {
				flush(i)
			}
			if start < 0 {
				start = i
			}
		}
		flush(len(toks))
	}
	sort.Strings(out)
	return out
}

// MarshalReferenceMatches renders the report as the staged JSON.
func MarshalReferenceMatches(r *ReferenceMatches) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
