package spelling

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// --- helpers ---------------------------------------------------------------

func writeLayer(t *testing.T, work, sub string, chapters map[int]string) {
	t.Helper()
	dir := filepath.Join(work, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, c := range chapters {
		p := filepath.Join(dir, transcript.TextName(n))
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeCorrected(t *testing.T, work string, chapters map[int]string) {
	t.Helper()
	writeLayer(t, work, CorrectedDir, chapters)
}

func writeText(t *testing.T, work string, chapters map[int]string) {
	t.Helper()
	writeLayer(t, work, transcript.TextDir, chapters)
}

func readCorrected(t *testing.T, work string, n int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(work, CorrectedDir, fmt.Sprintf("ch%03d.txt", n)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func findFailure(r *CheckResult, kind CheckFailureKind) *CheckFailure {
	for i := range r.Failures {
		if r.Failures[i].Kind == kind {
			return &r.Failures[i]
		}
	}
	return nil
}

// --- Occurrences -----------------------------------------------------------

func TestOccurrences(t *testing.T) {
	cases := []struct {
		name, hay, term string
		want            int
	}{
		{"empty term", "hello", "", 0},
		{"at start", "Aston is here", "Aston", 1},
		{"at end", "here is Aston", "Aston", 1},
		{"only occurrence whole string", "Aston", "Aston", 1},
		{"preceded by letter not counted", "XAston here", "Aston", 0},
		{"followed by letter not counted", "Astonish", "Aston", 0},
		{"followed by digit counted", "Aston3 units", "Aston", 1},
		{"followed by punctuation counted", "Aston. Next", "Aston", 1},
		{"followed by apostrophe counted", "Aston's blade", "Aston", 1},
		// The d'Daston pitfall: a word boundary matches inside an apostrophe, so a
		// \b-based count would (wrongly) find Aston here - and Occurrences does too,
		// which is the point: the apostrophe is a non-letter, so Aston is bounded.
		{"inside an apostrophe cluster", "Countess d'Aston", "Aston", 1},
		// A term ending in an apostrophe - impossible for \b, fine for the lookaround.
		{"term ending in apostrophe", "the O' clock tower", "O'", 1},
		{"multi-word term", "the Book of Pages here", "Book of", 1},
		{"multi-word term not bounded", "aBook of Pages", "Book of", 0},
		{"two occurrences", "Aston then Aston", "Aston", 2},
		// Non-overlapping advance: "x x x x" contains "x x" at 0/2/4 overlapping, but
		// only 2 non-overlapping (start 0, then start 4) survive the boundary + advance.
		{"non-overlapping advance", "x x x x", "x x", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Occurrences(c.hay, c.term); got != c.want {
				t.Errorf("Occurrences(%q, %q) = %d, want %d", c.hay, c.term, got, c.want)
			}
		})
	}
}

// --- deriveBase (gate 2/3 possessive + $1 prefix) --------------------------

func TestDeriveBase(t *testing.T) {
	cases := []struct{ repl, want string }{
		{"$1 Daston", "Daston"},   // title-preserving phrase rule
		{"Celaine's", "Celaine"},  // possessive
		{"$1 Owalyn's", "Owalyn"}, // both prefix and possessive
		{"Daston", "Daston"},      // bare
		{"Nordric", "Nordric"},    // no-op
	}
	for _, c := range cases {
		if got := deriveBase(c.repl); got != c.want {
			t.Errorf("deriveBase(%q) = %q, want %q", c.repl, got, c.want)
		}
	}
}

// --- Apply: rule ordering is a hard contract -------------------------------

func TestApplyRuleOrderingHonored(t *testing.T) {
	phrase := Rule{Pattern: `New York`, Replacement: `NYC`, Note: "phrase"}
	bare := Rule{Pattern: `\bYork\b`, Replacement: `Ork`, Note: "bare"}
	input := map[int]string{1: "I visited New York today."}

	// Phrase before bare: the phrase wins, no bare "York" is left to rewrite.
	workA := t.TempDir()
	writeText(t, workA, input)
	if _, err := Apply(workA, &Corrections{Rules: []Rule{phrase, bare}}); err != nil {
		t.Fatal(err)
	}
	if got := readCorrected(t, workA, 1); !strings.Contains(got, "NYC") || strings.Contains(got, "Ork") {
		t.Errorf("phrase-first: got %q, want NYC and no Ork", got)
	}

	// Bare before phrase: the bare rule fires first and the phrase never matches.
	workB := t.TempDir()
	writeText(t, workB, input)
	if _, err := Apply(workB, &Corrections{Rules: []Rule{bare, phrase}}); err != nil {
		t.Fatal(err)
	}
	if got := readCorrected(t, workB, 1); !strings.Contains(got, "New Ork") || strings.Contains(got, "NYC") {
		t.Errorf("bare-first: got %q, want 'New Ork' and no NYC", got)
	}
}

// --- The d'Daston phantom-split regression ---------------------------------

func TestDDastonForgeryAndCleanRuleSet(t *testing.T) {
	// Forgery: a bare \bAston\b -> Daston rule matches INSIDE d'Aston (an apostrophe
	// is a non-word char, so the boundary sits there) and forges "d'Daston".
	forge := t.TempDir()
	writeText(t, forge, map[int]string{1: "Countess d'Aston arrived."})
	bareRules := &Corrections{Rules: []Rule{{Pattern: `\bAston\b`, Replacement: `Daston`, Note: "bare"}}}
	if _, err := Apply(forge, bareRules); err != nil {
		t.Fatal(err)
	}
	if got := readCorrected(t, forge, 1); !strings.Contains(got, "d'Daston") {
		t.Fatalf("expected the forged 'd'Daston' in the corrected layer, got %q", got)
	}
	res, err := Check(forge, bareRules)
	if err != nil {
		t.Fatal(err)
	}
	if f := findFailure(res, FailPhantomNoble); f == nil {
		t.Errorf("gate 4 did not catch the phantom noble; failures: %v", res.Failures)
	} else if !strings.Contains(f.Message, "PARTICLE NOBLES SURVIVE") {
		t.Errorf("phantom failure message = %q", f.Message)
	}

	// The correct rule set: whole phrases first ($1 keeps the title), then the
	// lookbehind bare-surname rule. Produces clean "Countess Daston" and passes.
	clean := t.TempDir()
	writeText(t, clean, map[int]string{1: "Countess d'Aston arrived at the de Aston estate."})
	cleanRules := &Corrections{Rules: []Rule{
		{Pattern: `\b(Countess|Count|Lady|Lord)\s+(?:de|d')\s?[A-Z]\w*`, Replacement: `$1 Daston`, Note: "phantom family"},
		{Pattern: `(?<!\w)(?:de|d')\s?(?:Aston|Astan|Eston)\b`, Replacement: `Daston`, Note: "bare surname"},
	}}
	if _, err := Apply(clean, cleanRules); err != nil {
		t.Fatal(err)
	}
	got := readCorrected(t, clean, 1)
	if !strings.Contains(got, "Countess Daston") || strings.Contains(got, "d'Daston") || strings.Contains(got, "d'Aston") {
		t.Fatalf("clean rule set produced %q", got)
	}
	res, err = Check(clean, cleanRules)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ok() {
		t.Errorf("clean rule set should pass every gate; got %s", res.Summary())
	}
}

// --- The Owalyn forgery: gate 3 (attestation) ------------------------------

func TestOwalynForgeryGate3(t *testing.T) {
	rules := &Corrections{Rules: []Rule{{Pattern: `\bOwlin\b`, Replacement: `Owalyn`, Note: "goddess"}}}

	// (a) The RHS "Owalyn" is attested NOWHERE (not in the layer, no reference
	// files): gate 3 fires with the INVENTED wording (gate 2 also fires - dead rule).
	forge := t.TempDir()
	writeCorrected(t, forge, map[int]string{1: "The temple stood empty."})
	res, err := Check(forge, rules)
	if err != nil {
		t.Fatal(err)
	}
	f := findFailure(res, FailRHSUnattested)
	if f == nil {
		t.Fatalf("gate 3 did not fire; failures: %v", res.Failures)
	}
	if !strings.Contains(f.Message, "A RULE MAY HAVE INVENTED IT") {
		t.Errorf("gate 3 message = %q, want the INVENTED wording", f.Message)
	}

	// (b) A reference file attests "Owalyn": gate 3 no longer fires (it is attested
	// externally), even though the name is still absent from the layer (gate 2 fires).
	attested := t.TempDir()
	writeCorrected(t, attested, map[int]string{1: "The temple stood empty."})
	if err := os.WriteFile(filepath.Join(attested, "ref.txt"), []byte("the goddess Owalyn watches"), 0o644); err != nil {
		t.Fatal(err)
	}
	rulesRef := &Corrections{
		Rules:          rules.Rules,
		ReferenceFiles: []string{"ref.txt"},
	}
	res, err = Check(attested, rulesRef)
	if err != nil {
		t.Fatal(err)
	}
	if f := findFailure(res, FailRHSUnattested); f != nil {
		t.Errorf("gate 3 should pass when a reference file attests the name; got %q", f.Message)
	}

	// (c) The rule actually fires (the layer contains Owalyn) and it is attested:
	// every gate passes.
	clean := t.TempDir()
	writeText(t, clean, map[int]string{1: "The priestess Owlin spoke to Owalyn."})
	if err := os.WriteFile(filepath.Join(clean, "ref.txt"), []byte("the goddess Owalyn"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(clean, rulesRef); err != nil {
		t.Fatal(err)
	}
	res, err = Check(clean, rulesRef)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ok() {
		t.Errorf("clean+attested should pass; got %s", res.Summary())
	}
}

// --- Gate 1: an LHS variant survives the layer -----------------------------

func TestCheckGate1LHSSurvives(t *testing.T) {
	// The layer still contains the LHS "Owlin" (as if RULES were edited but Apply
	// was never re-run - the Book 2 cascade). Gate 1 must catch it.
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{1: "Owlin and Owalyn both appear."})
	rules := &Corrections{Rules: []Rule{{Pattern: `\bOwlin\b`, Replacement: `Owalyn`, Note: "goddess"}}}
	res, err := Check(work, rules)
	if err != nil {
		t.Fatal(err)
	}
	f := findFailure(res, FailLHSSurvives)
	if f == nil {
		t.Fatalf("gate 1 did not fire; failures: %v", res.Failures)
	}
	if !strings.Contains(f.Message, "LHS SURVIVES") {
		t.Errorf("gate 1 message = %q", f.Message)
	}
}

// --- Gate 2: a dead rule (RHS never occurs) --------------------------------

func TestCheckGate2DeadRule(t *testing.T) {
	// The corrected layer never contains "Vamir" - the rule's cast is absent.
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{1: "Only Hump is here."})
	// Attest the RHS in a reference file so gate 3 does NOT also fire, isolating gate 2.
	if err := os.WriteFile(filepath.Join(work, "ref.txt"), []byte("Vamir of the old books"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := &Corrections{
		Rules:          []Rule{{Pattern: `\bVameer\b`, Replacement: `Vamir`, Note: "series"}},
		ReferenceFiles: []string{"ref.txt"},
	}
	res, err := Check(work, rules)
	if err != nil {
		t.Fatal(err)
	}
	if f := findFailure(res, FailRHSAbsent); f == nil {
		t.Fatalf("gate 2 did not fire; failures: %v", res.Failures)
	} else if !strings.Contains(f.Message, "dead rule") {
		t.Errorf("gate 2 message = %q", f.Message)
	}
	if f := findFailure(res, FailRHSUnattested); f != nil {
		t.Errorf("gate 3 should pass (attested in ref.txt); got %q", f.Message)
	}
}

// --- Possessive base is checked, layer self-attests ------------------------

func TestCheckPossessiveBase(t *testing.T) {
	// The layer contains "Celaine's"; the derived base "Celaine" is bounded by the
	// apostrophe (Occurrences counts it), so gates 2/3 pass with no external ref.
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{1: "That was Celaine's blade."})
	rules := &Corrections{Rules: []Rule{{Pattern: `\bSelene's\b`, Replacement: `Celaine's`, Note: "possessive"}}}
	res, err := Check(work, rules)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ok() {
		t.Errorf("possessive base should pass; got %s", res.Summary())
	}
}

// --- Apply prefers the repaired layer + exact corrections.log --------------

func TestApplyPrefersRepairedLayer(t *testing.T) {
	work := t.TempDir()
	writeText(t, work, map[int]string{1: "raw text York"})
	writeLayer(t, work, transcript.RepairedDir, map[int]string{1: "repaired York"})
	res, err := Apply(work, &Corrections{Rules: []Rule{{Pattern: `\bYork\b`, Replacement: `Ork`, Note: "n"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := readCorrected(t, work, 1); got != "repaired Ork" {
		t.Errorf("corrected = %q, want 'repaired Ork' (repaired layer preferred)", got)
	}
	if len(res.PerChapter) != 1 || res.PerChapter[0].Source != "repaired" {
		t.Errorf("PerChapter = %+v, want one entry sourced 'repaired'", res.PerChapter)
	}
}

func TestApplyCorrectionsLogGolden(t *testing.T) {
	work := t.TempDir()
	writeText(t, work, map[int]string{1: "York and foo", 2: "nothing here"})
	rules := &Corrections{Rules: []Rule{
		{Pattern: `\bYork\b`, Replacement: `Ork`, Note: "n1"},
		{Pattern: `\bfoo\b`, Replacement: `bar`, Note: "n2"},
	}}
	res, err := Apply(work, rules)
	if err != nil {
		t.Fatal(err)
	}
	want := `# corrections log (raw + text layers are immutable)

chapters: 2, replacements: 2, rules fired: 2/2

- \bYork\b -> Ork  x1  # n1
- \bfoo\b -> bar  x1  # n2

## per chapter
- ch001 (text): 2
`
	if res.Log != want {
		t.Errorf("corrections.log mismatch:\n--- got ---\n%s\n--- want ---\n%s", res.Log, want)
	}
	// The log is also written to disk.
	onDisk, err := os.ReadFile(filepath.Join(work, "corrections.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != want {
		t.Errorf("corrections.log on disk differs from ApplyResult.Log")
	}
}

// --- GenerateSheets: chunk gating, carryover, and the md golden ------------

func sheetFixture() *Spellings {
	return &Spellings{
		Title:     "TEST spellings",
		Preamble:  []string{"A preamble line."},
		ChunkEnds: []int{2, 4},
		Ledger: []LedgerEntry{
			{Canonical: "Alpha", Type: "person", Status: "verified", Carryover: true, Variants: "-", Note: "known"},
			{Canonical: "Beta", Type: "person", Status: "probable", Carryover: false, Variants: "Beeta", Note: "as-heard"},
			{Canonical: "Gamma", Type: "person", Status: "probable", Carryover: false, Variants: "-", Note: "late"},
		},
		Unresolved: []string{"Zeta"},
		Clusters:   []Cluster{{Names: []string{"Beta", "Gamma"}, Text: "- Beta vs Gamma"}},
		NonMerges:  []NonMerge{{A: "Alpha", B: "Beta", Text: "- Alpha vs Beta"}},
	}
}

func sheetLayer(t *testing.T, work string) {
	writeCorrected(t, work, map[int]string{
		1: "Alpha appears.",
		2: "Beta and Zeta appear.",
		3: "Nothing.",
		4: "Gamma appears.",
	})
}

func TestGenerateSheetsChunkGatingAndGolden(t *testing.T) {
	work := t.TempDir()
	sheetLayer(t, work)
	res, err := GenerateSheets(work, sheetFixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sheets) != 2 {
		t.Fatalf("want 2 sheets, got %d", len(res.Sheets))
	}
	// ch2: Alpha (carryover fu0) + Beta (fu2); Gamma (fu4) excluded. Cluster not yet
	// applicable (Gamma unheard). Non-merge Alpha vs Beta shown.
	ch2 := res.Sheets[0]
	if ch2.Rows != 2 || ch2.Unresolved != 1 || ch2.Clusters != 0 {
		t.Errorf("ch2 stats = %+v, want rows 2 / unresolved 1 / clusters 0", ch2)
	}
	// ch4: Gamma now included (3 rows); cluster [Beta,Gamma] now shown.
	ch4 := res.Sheets[1]
	if ch4.Rows != 3 || ch4.Clusters != 1 {
		t.Errorf("ch4 stats = %+v, want rows 3 / clusters 1", ch4)
	}

	gotMD, err := os.ReadFile(filepath.Join(work, FactsDir, "spellings-through-ch2.md"))
	if err != nil {
		t.Fatal(err)
	}
	wantMD := `# TEST spellings - through chapter 2

A preamble line.

| canonical | type | status | first_use | ASR variants seen | note |
| --- | --- | --- | --- | --- | --- |
| **Alpha** | person | verified | ch0 | - | known |
| **Beta** | person | probable | ch2 | Beeta | as-heard |

## Unresolved / do-not-publish-clean (heard but ambiguous or invented)

NEVER publish these as clean names. Describe the figure BY ROLE or surname, or omit.

  Zeta

## DO-NOT-MERGE clusters (look-alike ASR strings that are DISTINCT entities)

(none applicable yet)

## Deliberate non-merges (carryover pairs the ASR blurs)

- Alpha vs Beta
`
	if string(gotMD) != wantMD {
		t.Errorf("ch2 sheet mismatch:\n--- got ---\n%s\n--- want ---\n%s", gotMD, wantMD)
	}
}

// --- GenerateSheets gate 1: a non-carryover term never heard ---------------

func TestGenerateSheetsGate1Missing(t *testing.T) {
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{1: "Alpha only."})
	data := &Spellings{
		Title:     "T",
		ChunkEnds: []int{2},
		Ledger: []LedgerEntry{
			{Canonical: "Alpha", Carryover: true},
			{Canonical: "Gorvol", Carryover: false}, // marker-only, ASR never produced it
		},
	}
	_, err := GenerateSheets(work, data)
	if err == nil || !strings.Contains(err.Error(), "gate 1") || !strings.Contains(err.Error(), "Gorvol") {
		t.Fatalf("want gate 1 error naming Gorvol, got %v", err)
	}
}

// --- GenerateSheets gate 2: a note leaks a later name ----------------------

func TestGenerateSheetsGate2SpoilerLeak(t *testing.T) {
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{1: "Early here.", 15: "Late here."})
	data := &Spellings{
		Title:     "T",
		ChunkEnds: []int{8},
		Ledger: []LedgerEntry{
			{Canonical: "Early", Carryover: true, Note: "related to Late"},
			{Canonical: "Late", Carryover: false, Note: "-"},
		},
	}
	_, err := GenerateSheets(work, data)
	if err == nil || !strings.Contains(err.Error(), "gate 2") || !strings.Contains(err.Error(), "first heard ch15") {
		t.Fatalf("want gate 2 spoiler-leak error, got %v", err)
	}
}

// --- GenerateSheets: unresolved gating + the (first_use or 0) ch0 quirk -----

func TestGenerateSheetsUnresolvedGating(t *testing.T) {
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{2: "Zeta here.", 4: "Later here."})
	data := &Spellings{
		Title:      "T",
		ChunkEnds:  []int{2, 4},
		Ledger:     []LedgerEntry{{Canonical: "Zeta", Carryover: false}},
		Unresolved: []string{"Zeta", "Later"},
	}
	// Gate 1 needs Zeta (non-carryover) heard - it is (ch2).
	res, err := GenerateSheets(work, data)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sheets[0].Unresolved != 1 { // ch2: only Zeta
		t.Errorf("ch2 unresolved = %d, want 1", res.Sheets[0].Unresolved)
	}
	if res.Sheets[1].Unresolved != 2 { // ch4: Zeta + Later
		t.Errorf("ch4 unresolved = %d, want 2", res.Sheets[1].Unresolved)
	}
}

func TestGenerateSheetsNonMergeCh0Quirk(t *testing.T) {
	// A ledger canonical first heard in chapter 0 (front matter). Python's
	// (first_use or 0) makes 0 falsy, but the guard reduces to first_use <= end, so a
	// ch0 term is still "present" and the non-merge shows.
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{0: "Nisha and Nishari.", 1: "story begins"})
	data := &Spellings{
		Title:     "T",
		ChunkEnds: []int{1},
		Ledger: []LedgerEntry{
			{Canonical: "Nisha", Carryover: true},
			{Canonical: "Nishari", Carryover: true},
		},
		NonMerges: []NonMerge{{A: "Nisha", B: "Nishari", Text: "- Nisha vs Nishari"}},
	}
	if _, err := GenerateSheets(work, data); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(work, FactsDir, "spellings-through-ch1.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "- Nisha vs Nishari") {
		t.Errorf("ch0-heard non-merge pair should be present at ch1; sheet:\n%s", md)
	}
}

// --- CheckFirstUse ---------------------------------------------------------

func TestCheckFirstUse(t *testing.T) {
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{
		1: "Carwyn met O'Brien.",
		2: "Okello and Lateman arrived.",
		3: "Earlyn appeared.",
	})
	sheet := filepath.Join(work, "roster.md")
	body := strings.Join([]string{
		"**Okello** - first appearance: ch2",
		"**Lateman** - first appearance: ch5",
		"**Earlyn** - first appearance: ch1",
		"**Carwyn** first appearance in books 1-4",
		"**Ghostly** - first appearance: ch2",
		"**O'Brien** - first appearance: ch1",
	}, "\n")
	if err := os.WriteFile(sheet, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := CheckFirstUse(work, sheet)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Verdict{
		"Okello":  VerdictOK,
		"Lateman": VerdictProblem,
		"Earlyn":  VerdictAdvisory,
		"Carwyn":  VerdictCarryover,
		"Ghostly": VerdictNotNamed,
		"O'Brien": VerdictOK,
	}
	if len(res.Rows) != len(want) {
		t.Fatalf("parsed %d rows, want %d: %+v", len(res.Rows), len(want), res.Rows)
	}
	for _, row := range res.Rows {
		if w, ok := want[row.Name]; !ok {
			t.Errorf("unexpected row %q", row.Name)
		} else if row.Verdict != w {
			t.Errorf("%s verdict = %q, want %q", row.Name, row.Verdict, w)
		}
	}
	if res.Problems != 1 {
		t.Errorf("Problems = %d, want 1 (Lateman)", res.Problems)
	}
}

func TestCheckFirstUseZeroRows(t *testing.T) {
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{1: "text"})
	sheet := filepath.Join(work, "roster.md")
	if err := os.WriteFile(sheet, []byte("no roster rows here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckFirstUse(work, sheet); err == nil {
		t.Error("want an error when no roster rows parse")
	}
}

// --- Load* validation ------------------------------------------------------

func TestLoadCorrectionsMissingSentinel(t *testing.T) {
	if _, err := LoadCorrections(t.TempDir()); !errors.Is(err, ErrNoCorrections) {
		t.Errorf("missing corrections.json: err = %v, want ErrNoCorrections", err)
	}
}

func TestLoadSpellingsMissingSentinel(t *testing.T) {
	if _, err := LoadSpellings(t.TempDir()); !errors.Is(err, ErrNoSpellings) {
		t.Errorf("missing spellings.json: err = %v, want ErrNoSpellings", err)
	}
}

func TestLoadCorrectionsBadPattern(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, CorrectionsFile),
		[]byte(`{"rules":[{"pattern":"(","replacement":"x"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCorrections(work)
	if err == nil || errors.Is(err, ErrNoCorrections) {
		t.Errorf("bad pattern should be a validation error, got %v", err)
	}
}

func TestLoadCorrectionsEmptyReplacement(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, CorrectionsFile),
		[]byte(`{"rules":[{"pattern":"\\bX\\b","replacement":""}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCorrections(work); err == nil {
		t.Error("empty replacement should be rejected")
	}
}

func TestLoadSpellingsChunkEndsValidation(t *testing.T) {
	cases := map[string]string{
		"unordered":   `{"chunk_ends":[8,8],"ledger":[{"canonical":"A"}]}`,
		"decreasing":  `{"chunk_ends":[18,8],"ledger":[{"canonical":"A"}]}`,
		"nonpositive": `{"chunk_ends":[0,8],"ledger":[{"canonical":"A"}]}`,
		"empty canon": `{"chunk_ends":[8],"ledger":[{"canonical":"  "}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			if err := os.WriteFile(filepath.Join(work, SpellingsFile), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadSpellings(work); err == nil {
				t.Errorf("expected a validation error for %s", name)
			}
		})
	}
}

func TestLoadSpellingsValid(t *testing.T) {
	work := t.TempDir()
	body := `{"title":"T","chunk_ends":[8,18],"ledger":[{"canonical":"Alpha","carryover":true}]}`
	if err := os.WriteFile(filepath.Join(work, SpellingsFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSpellings(work)
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "T" || len(s.ChunkEnds) != 2 || len(s.Ledger) != 1 {
		t.Errorf("loaded spellings = %+v", s)
	}
}

// --- reference directory ordering (NaturalLess) ----------------------------

func TestReferenceDirectoryAttestation(t *testing.T) {
	// A reference_files entry that is a DIRECTORY contributes all its *.txt files,
	// so a name attested only in the mirror passes gate 3 while absent from the layer
	// (gate 2 still fires - dead rule - which we tolerate here).
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{1: "Hump alone."})
	mirror := filepath.Join(work, "mirror")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "p2.txt"), []byte("nothing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "p10.txt"), []byte("Alveron the kingdom"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := &Corrections{
		Rules:          []Rule{{Pattern: `\bAlvaron\b`, Replacement: `Alveron`, Note: "series"}},
		ReferenceFiles: []string{"mirror"},
	}
	res, err := Check(work, rules)
	if err != nil {
		t.Fatal(err)
	}
	if f := findFailure(res, FailRHSUnattested); f != nil {
		t.Errorf("gate 3 should pass (mirror attests Alveron); got %q", f.Message)
	}
}

// The gates can only attest a literal name, so Validate refuses replacement group
// syntax other than a leading "$1 " title prefix ("$$" literal-dollar is allowed).
func TestValidateReplacementGroupSyntax(t *testing.T) {
	for _, tc := range []struct {
		repl string
		ok   bool
	}{
		{"Daston", true},
		{"$1 Daston", true},
		{"costs $$5", true},
		{"${1} Daston", false},
		{"$2 Daston", false},
		{"Daston $1", false},
	} {
		c := Corrections{Rules: []Rule{{Pattern: `\bX\b`, Replacement: tc.repl}}}
		err := c.Validate()
		if tc.ok && err != nil {
			t.Errorf("replacement %q should validate, got %v", tc.repl, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("replacement %q should be rejected", tc.repl)
		}
	}
}
