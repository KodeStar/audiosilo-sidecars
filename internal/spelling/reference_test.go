package spelling

import "testing"

// candidatesFrom builds a Candidates from (form, count) pairs.
func candidatesFrom(pairs ...any) *Candidates {
	c := &Candidates{}
	for i := 0; i < len(pairs); i += 2 {
		c.Candidates = append(c.Candidates, Candidate{
			Form:  pairs[i].(string),
			Count: pairs[i+1].(int),
		})
	}
	return c
}

func glossary(names ...string) ReferenceSource {
	return ReferenceSource{Name: "spelling-refs/series-glossary.txt", Authority: AuthorityVerified, Names: names}
}

func matchFor(r *ReferenceMatches, form string) (ReferenceMatch, bool) {
	for _, m := range r.Matches {
		if m.Form == form {
			return m, true
		}
	}
	return ReferenceMatch{}, false
}

// TestBuildReferenceMatchesRealMisspellings pins the six Wandering Inn names the
// audio pipeline actually got wrong. Each was self-consistent across its whole book
// (so no intra-transcript signal existed) and each is one or two edits from the
// canonical spelling the community database records.
func TestBuildReferenceMatchesRealMisspellings(t *testing.T) {
	cases := []struct {
		form, canonical string
		wantDistance    int
	}{
		{"Torrin", "Toren", 2},
		{"Floss", "Flos", 1},
		{"Terriarch", "Teriarch", 1},
		{"Goddard", "Godart", 2},
		{"Prognogator", "Prognugator", 1},
		{"Valsaif Godfrey", "Valceif Godfrey", 2},
	}
	for _, tc := range cases {
		t.Run(tc.form, func(t *testing.T) {
			got := BuildReferenceMatches(candidatesFrom(tc.form, 300), []ReferenceSource{glossary(tc.canonical)})
			m, ok := matchFor(got, tc.form)
			if !ok {
				t.Fatalf("%q was not proposed against %q", tc.form, tc.canonical)
			}
			if m.Reference != tc.canonical {
				t.Errorf("reference = %q, want %q", m.Reference, tc.canonical)
			}
			if m.Distance != tc.wantDistance {
				t.Errorf("distance = %d, want %d", m.Distance, tc.wantDistance)
			}
			if m.Authority != AuthorityVerified {
				t.Errorf("authority = %q, want verified", m.Authority)
			}
		})
	}
}

// TestBuildReferenceMatchesKnownFormNeverProposed is the precision rule that keeps
// two genuinely distinct near-identical names apart. Terra (a goddess) and Tersa (a
// guard recruit) are one edit apart and BOTH real - the AI reviewer flagged them as
// a typo for each other and was wrong. Because both are in the glossary, neither
// may be proposed as a correction of the other.
func TestBuildReferenceMatchesKnownFormNeverProposed(t *testing.T) {
	got := BuildReferenceMatches(
		candidatesFrom("Tersa", 40, "Terra", 12),
		[]ReferenceSource{glossary("Terra", "Tersa")},
	)
	if !got.Empty() {
		t.Fatalf("both names are attested; nothing should be proposed, got %+v", got.Matches)
	}
}

// TestBuildReferenceMatchesVerifiedOutranksCarryover is the precedence rule. The
// previous volume's corrected text carries its own mistake verbatim (Book 5 held
// "Floss" 1135 times and "Flos" zero), so a carryover source must never win over a
// verified one at equal distance.
func TestBuildReferenceMatchesVerifiedOutranksCarryover(t *testing.T) {
	got := BuildReferenceMatches(
		candidatesFrom("Flosz", 900),
		[]ReferenceSource{
			{Name: "spelling-refs/prior-spellings.json", Authority: AuthorityCarryover, Names: []string{"Floss"}},
			glossary("Flos"),
		},
	)
	m, ok := matchFor(got, "Flosz")
	if !ok {
		t.Fatal("no proposal for Flosz")
	}
	if m.Authority != AuthorityVerified || m.Reference != "Flos" {
		t.Errorf("got %q from %q (%s), want Flos from the verified glossary", m.Reference, m.Source, m.Authority)
	}
}

// TestBuildReferenceMatchesCarryoverNeverSettlesAForm is the regression guard for
// the propagation mechanism itself. Book 5 spelled the skeleton "Torrin" 369 times
// and its ledger records that as canonical, so Book 6's carryover asserts "Torrin"
// exactly. If a carryover match counted as "the references already spell it this
// way", the propagated error would immunise itself and the glossary's correction
// would never be proposed - which is how the mistake reached a whole series.
func TestBuildReferenceMatchesCarryoverNeverSettlesAForm(t *testing.T) {
	got := BuildReferenceMatches(
		candidatesFrom("Torrin", 369),
		[]ReferenceSource{
			{Name: "spelling-refs/prior-spellings.json", Authority: AuthorityCarryover, Names: []string{"Torrin"}},
			glossary("Toren"),
		},
	)
	m, ok := matchFor(got, "Torrin")
	if !ok {
		t.Fatal("the carryover suppressed the glossary's correction - the propagation bug is back")
	}
	if m.Reference != "Toren" || m.Authority != AuthorityVerified {
		t.Errorf("proposal = %+v, want Toren from the verified glossary", m)
	}
}

// TestBuildReferenceMatchesNoSelfProposal: with only a carryover source that agrees
// with the transcript, there is no difference to report.
func TestBuildReferenceMatchesNoSelfProposal(t *testing.T) {
	got := BuildReferenceMatches(
		candidatesFrom("Torrin", 369),
		[]ReferenceSource{{Name: "spelling-refs/prior-spellings.json", Authority: AuthorityCarryover, Names: []string{"Torrin"}}},
	)
	if !got.Empty() {
		t.Errorf("an exact carryover agreement is not a correction: %+v", got.Matches)
	}
}

// TestBuildReferenceMatchesCarryoverStillReported: with no verified source the
// carryover proposal is still surfaced, but labelled so the prompt can rank it.
func TestBuildReferenceMatchesCarryoverStillReported(t *testing.T) {
	got := BuildReferenceMatches(
		candidatesFrom("Torrn", 30),
		[]ReferenceSource{{Name: "spelling-refs/prior-spellings.json", Authority: AuthorityCarryover, Names: []string{"Torrin"}}},
	)
	m, ok := matchFor(got, "Torrn")
	if !ok {
		t.Fatal("carryover proposal missing")
	}
	if m.Authority != AuthorityCarryover {
		t.Errorf("authority = %q, want carryover", m.Authority)
	}
}

// TestBuildReferenceMatchesIgnoresLowercaseWords: the candidate extractor lists rare
// lowercase tokens, and ordinary English words land one or two edits from real
// character names. Rewriting a verb into a proper noun is not this pass's job. All
// four of these were proposed on a real book before the proper-noun gate.
func TestBuildReferenceMatchesIgnoresLowercaseWords(t *testing.T) {
	got := BuildReferenceMatches(
		candidatesFrom("glared", 66, "grunted", 46, "mass", 24, "heroes", 23),
		[]ReferenceSource{glossary("Garen", "Grunter", "Mars", "Hero")},
	)
	if !got.Empty() {
		t.Errorf("lowercase words must not be corrected into names: %+v", got.Matches)
	}
}

// TestBuildReferenceMatchesSettlesNamePartsFromMultiWordNames: the vocabularies
// only ever emit whole names ("Laken Godart"), but a transcript names a character
// by first name constantly. Without indexing the parts, a correctly spelled "Erin"
// is settled by nothing and gets proposed as a misspelling of some OTHER name.
func TestBuildReferenceMatchesSettlesNamePartsFromMultiWordNames(t *testing.T) {
	got := BuildReferenceMatches(
		candidatesFrom("Erin", 500),
		[]ReferenceSource{glossary("Erin Solstice", "Erina")},
	)
	if _, ok := matchFor(got, "Erin"); ok {
		t.Errorf("Erin is part of an attested name; it must not be proposed: %+v", got.Matches)
	}
}

// TestBuildReferenceMatchesMatchesAgainstNameParts is the recall half: the glossary
// holds "Laken Godart" but the transcript mishears the surname alone, and a
// whole-name comparison never gets past the first letter.
func TestBuildReferenceMatchesMatchesAgainstNameParts(t *testing.T) {
	got := BuildReferenceMatches(
		candidatesFrom("Goddard", 300),
		[]ReferenceSource{glossary("Laken Godart")},
	)
	m, ok := matchFor(got, "Goddard")
	if !ok {
		t.Fatalf("no proposal for Goddard against the surname in %q", "Laken Godart")
	}
	if m.Reference != "Godart" {
		t.Errorf("reference = %q, want the surname Godart", m.Reference)
	}
}

// TestBuildReferenceMatchesPluralNeedsEvidence: "Godarts" over an attested "Godart"
// is a plural and must not be proposed - but "Floss" over an attested "Flos" has the
// same shape and IS the misspelling this whole feature exists to catch. The
// tiebreaker is the transcript: a real plural implies the singular occurs too.
func TestBuildReferenceMatchesPluralNeedsEvidence(t *testing.T) {
	t.Run("plural suppressed when the singular is in the transcript", func(t *testing.T) {
		got := BuildReferenceMatches(
			candidatesFrom("Godarts", 30, "Godart", 300),
			[]ReferenceSource{glossary("Godart")},
		)
		if _, ok := matchFor(got, "Godarts"); ok {
			t.Errorf("a genuine plural must not be proposed: %+v", got.Matches)
		}
	})
	t.Run("misspelling kept when the singular never occurs", func(t *testing.T) {
		got := BuildReferenceMatches(
			candidatesFrom("Floss", 1135),
			[]ReferenceSource{glossary("Flos")},
		)
		if _, ok := matchFor(got, "Floss"); !ok {
			t.Errorf("Floss is not a plural of Flos - the transcript never says Flos: %+v", got.Matches)
		}
	})
}

// TestBuildReferenceMatchesIgnoresContractionsAndStrayTokens covers two shapes the
// extractor emits that are one or two edits from a real name but are not
// misspellings of it. Both were proposed on a real book before these gates.
func TestBuildReferenceMatchesIgnoresContractionsAndStrayTokens(t *testing.T) {
	t.Run("contraction", func(t *testing.T) {
		got := BuildReferenceMatches(candidatesFrom("He'd", 60), []ReferenceSource{glossary("Head")})
		if !got.Empty() {
			t.Errorf("a contraction is not a name: %+v", got.Matches)
		}
	})
	t.Run("known name plus a stray token", func(t *testing.T) {
		got := BuildReferenceMatches(
			candidatesFrom("Antinium I", 1, "Drake I", 1),
			[]ReferenceSource{glossary("Antinium", "Drake")},
		)
		if !got.Empty() {
			t.Errorf("an attested name with a stray token is that name: %+v", got.Matches)
		}
	})
	t.Run("a real two-word misspelling still proposes", func(t *testing.T) {
		got := BuildReferenceMatches(candidatesFrom("Bat Arrow", 16), []ReferenceSource{glossary("Bad Arrow")})
		if _, ok := matchFor(got, "Bat Arrow"); !ok {
			t.Errorf("the stray-token rule must not swallow a genuine phrase misspelling: %+v", got.Matches)
		}
	})
}

// TestNamesFromTitlesKeepsNamesBeforeAColon: slicing at the last colon dropped the
// character named in a POV-style title outright, and in a two-colon title replaced
// it with a junk noun.
func TestNamesFromTitlesKeepsNamesBeforeAColon(t *testing.T) {
	for _, tc := range []struct{ title, want string }{
		{"Toren: The Bridge", "Toren"},
		{"Chapter 12: Toren: A Reckoning", "Toren"},
		{"Chapter 9: Toren", "Toren"},
	} {
		got := NamesFromTitles(tc.title)
		found := false
		for _, n := range got {
			if n == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("NamesFromTitles(%q) = %v, missing %q", tc.title, got, tc.want)
		}
	}
}

// TestBuildReferenceMatchesIgnoresPossessives: the extractor lists "Magnolia's"
// separately from "Magnolia", and the possessive is two edits from the bare name -
// so without the possessive check a correctly spelled name is proposed as a
// misspelling of itself.
func TestBuildReferenceMatchesIgnoresPossessives(t *testing.T) {
	got := BuildReferenceMatches(
		candidatesFrom("Magnolia's", 61, "Pyrite's", 23),
		[]ReferenceSource{glossary("Magnolia", "Pyrite")},
	)
	if !got.Empty() {
		t.Errorf("a possessive of an attested name is not a misspelling: %+v", got.Matches)
	}
}

func TestBuildReferenceMatchesRejectsDistantAndShortForms(t *testing.T) {
	cases := []struct{ name, form, ref string }{
		{"different first letter", "Gorrin", "Toren"},
		{"too many edits", "Torrance", "Toren"},
		{"short form gets no budget", "Rag", "Rat"},
		{"unrelated", "Magnolia", "Klbkch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildReferenceMatches(candidatesFrom(tc.form, 10), []ReferenceSource{glossary(tc.ref)})
			if _, ok := matchFor(got, tc.form); ok {
				t.Errorf("%q should not be proposed against %q", tc.form, tc.ref)
			}
		})
	}
}

func TestBuildReferenceMatchesOrdersVerifiedFirstThenCount(t *testing.T) {
	got := BuildReferenceMatches(
		candidatesFrom("Lowcount", 5, "Highcount", 900, "Carryform", 5000),
		[]ReferenceSource{
			glossary("Lowcounts", "Highcounts"),
			{Name: "spelling-refs/prior-spellings.json", Authority: AuthorityCarryover, Names: []string{"Carryforms"}},
		},
	)
	if len(got.Matches) != 3 {
		t.Fatalf("want 3 proposals, got %d (%+v)", len(got.Matches), got.Matches)
	}
	if got.Matches[0].Form != "Highcount" || got.Matches[1].Form != "Lowcount" {
		t.Errorf("verified proposals should lead, ordered by count: %+v", got.Matches)
	}
	if got.Matches[2].Form != "Carryform" {
		t.Errorf("carryover proposal should sort last despite its count: %+v", got.Matches)
	}
}

func TestBuildReferenceMatchesNilAndEmpty(t *testing.T) {
	if got := BuildReferenceMatches(nil, []ReferenceSource{glossary("Toren")}); !got.Empty() {
		t.Error("nil candidates should propose nothing")
	}
	if got := BuildReferenceMatches(candidatesFrom("Torrin", 10), nil); !got.Empty() {
		t.Error("no sources should propose nothing")
	}
}

// TestBuildReferenceMatchesRecordsSources keeps the report self-describing: a
// consulted-but-empty source must still be listed, so "no glossary was available"
// is distinguishable from "the glossary agreed with everything".
func TestBuildReferenceMatchesRecordsSources(t *testing.T) {
	got := BuildReferenceMatches(candidatesFrom("Torrin", 10), []ReferenceSource{
		glossary("Toren"),
		{Name: "marker_titles.txt", Authority: AuthorityVerified},
	})
	if len(got.Sources) != 2 {
		t.Fatalf("want both sources recorded, got %+v", got.Sources)
	}
	if got.Sources[1].Name != "marker_titles.txt" || got.Sources[1].Names != 0 {
		t.Errorf("empty source should be recorded with zero names: %+v", got.Sources[1])
	}
}

func TestNamesFromTitles(t *testing.T) {
	// The-last-light's real marker table names the character in a chapter title;
	// Book 6's is a bare-number table and must yield nothing.
	// "Opening Credits" is structural marker boilerplate, not a character: admitting
	// "Credits" as a known spelling would both invite nonsense proposals and
	// suppress real ones.
	named := NamesFromTitles("Chapter 9: Toren\nChapter 10: Laken Godart\nOpening Credits\nEpilogue\n")
	want := map[string]bool{"Toren": true, "Laken Godart": true}
	for _, n := range named {
		if !want[n] {
			t.Errorf("unexpected name %q from titles", n)
		}
		delete(want, n)
	}
	if len(want) > 0 {
		t.Errorf("missing names from titles: %v (got %v)", want, named)
	}

	if got := NamesFromTitles("001\n002\n003\n027\n"); len(got) != 0 {
		t.Errorf("a bare-number marker table should yield no names, got %v", got)
	}
}

func TestLevenshteinLimit(t *testing.T) {
	if d := levenshtein("toren", "torrin", 2); d != 2 {
		t.Errorf("distance = %d, want 2", d)
	}
	if d := levenshtein("toren", "completely-different", 2); d != -1 {
		t.Errorf("distance = %d, want -1 (over limit)", d)
	}
	if d := levenshtein("same", "same", 2); d != 0 {
		t.Errorf("distance = %d, want 0", d)
	}
}
