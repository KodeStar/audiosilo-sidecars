package spelling

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// writeChapter writes a single chapter's text into <work>/<layer>/ (a thin wrapper
// over writeLayer, the shared multi-chapter helper in spelling_test.go).
func writeChapter(t *testing.T, work, layer string, chapter int, text string) {
	t.Helper()
	writeLayer(t, work, layer, map[int]string{chapter: text})
}

// candidateByForm returns the candidate with the given form, or nil.
func candidateByForm(c *Candidates, form string) *Candidate {
	for i := range c.Candidates {
		if c.Candidates[i].Form == form {
			return &c.Candidates[i]
		}
	}
	return nil
}

// letterSuffix maps an int to a lowercase letter string (a, b, ..., z, aa, ...) so
// tests can mint distinct letter-only tokens (digits are token boundaries).
func letterSuffix(i int) string {
	var b []byte
	for {
		b = append([]byte{byte('a' + i%26)}, b...)
		i = i/26 - 1
		if i < 0 {
			break
		}
	}
	return string(b)
}

func forms(c *Candidates) []string {
	out := make([]string, 0, len(c.Candidates))
	for _, cand := range c.Candidates {
		out = append(out, cand.Form)
	}
	sort.Strings(out)
	return out
}

func TestExtractCandidatesSentenceInitialExclusion(t *testing.T) {
	work := t.TempDir()
	// "Frost" occurs only sentence-initially (non_initial 0) -> excluded.
	// "Kael" occurs twice non-initially -> included.
	writeChapter(t, work, transcript.TextDir, 1,
		"Frost fell overnight. Frost returned by dawn. The captain saw Kael there. Kael waited, and Kael listened.")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	if candidateByForm(c, "Frost") != nil {
		t.Errorf("Frost is sentence-initial only and must be excluded; forms=%v", forms(c))
	}
	kael := candidateByForm(c, "Kael")
	if kael == nil {
		t.Fatalf("Kael should be a candidate (non_initial >= 2); forms=%v", forms(c))
	}
	if kael.NonInitial < 2 {
		t.Errorf("Kael non_initial = %d, want >= 2", kael.NonInitial)
	}
}

func TestExtractCandidatesMultiWordPhrase(t *testing.T) {
	work := t.TempDir()
	writeChapter(t, work, transcript.TextDir, 1,
		"They rode to Leafs Crossing at dawn. Beyond Leafs Crossing lay great danger.")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	lc := candidateByForm(c, "Leafs Crossing")
	if lc == nil {
		t.Fatalf("Leafs Crossing phrase should be a candidate; forms=%v", forms(c))
	}
	if lc.Count < 2 {
		t.Errorf("Leafs Crossing count = %d, want >= 2", lc.Count)
	}
	if lc.NonInitial != lc.Count {
		t.Errorf("phrase non_initial (%d) must equal count (%d)", lc.NonInitial, lc.Count)
	}
	// The sentence-opening common word "Beyond" must not chain into a junk phrase.
	if candidateByForm(c, "Beyond Leafs") != nil {
		t.Errorf("Beyond Leafs junk phrase should be trimmed; forms=%v", forms(c))
	}
}

func TestExtractCandidatesInternalCapitalApostrophe(t *testing.T) {
	work := t.TempDir()
	writeChapter(t, work, transcript.TextDir, 1,
		"The lady d'Aston smiled. Everyone feared d'Aston that day, and d'Aston knew it well.")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	d := candidateByForm(c, "d'Aston")
	if d == nil {
		t.Fatalf("d'Aston should be captured as one capitalized token; forms=%v", forms(c))
	}
	// It must never be split into a bare "Aston" (the d'Daston forgery shape).
	if candidateByForm(c, "Aston") != nil {
		t.Errorf("d'Aston must not split into a bare Aston token; forms=%v", forms(c))
	}
}

func TestExtractCandidatesLowercaseVariant(t *testing.T) {
	work := t.TempDir()
	writeChapter(t, work, transcript.TextDir, 1,
		"He feared the Blades and the Blades vanished. Later the blades were only common blades, dull blades.")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	if candidateByForm(c, "Blades") == nil {
		t.Fatalf("capitalized Blades should be included; forms=%v", forms(c))
	}
	lower := candidateByForm(c, "blades")
	if lower == nil {
		t.Fatalf("lowercase variant blades should be captured; forms=%v", forms(c))
	}
	if lower.Form != "blades" {
		t.Errorf("lowercase variant surface = %q, want exact lowercase form", lower.Form)
	}
}

func TestExtractCandidatesLowercasePhraseVariant(t *testing.T) {
	work := t.TempDir()
	writeChapter(t, work, transcript.TextDir, 1,
		"The Night Blades attacked at dusk. He feared the Night Blades. But the night blades were only shadows, mere night blades.")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	if candidateByForm(c, "Night Blades") == nil {
		t.Fatalf("Night Blades phrase should be included; forms=%v", forms(c))
	}
	lp := candidateByForm(c, "night blades")
	if lp == nil {
		t.Fatalf("lowercase phrase variant should be captured; forms=%v", forms(c))
	}
	if lp.Count < 2 {
		t.Errorf("night blades count = %d, want >= 2", lp.Count)
	}
}

func TestExtractCandidatesPhraseDoesNotSpanPunctuation(t *testing.T) {
	work := t.TempDir()
	// A comma-separated LIST of plants must NOT join "Nightshade Deathflower" into a
	// phantom phrase, while a plain-space name ("Leafs Crossing") still forms one.
	writeChapter(t, work, transcript.TextDir, 1,
		"The sprite said excited. Nightshade, Deathflower, and Shadowbane bloomed near Leafs Crossing. "+
			"They passed Nightshade, Deathflower again on the road to Leafs Crossing.")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	if candidateByForm(c, "Nightshade Deathflower") != nil {
		t.Errorf("a comma-separated list must not form the phantom phrase %q; forms=%v", "Nightshade Deathflower", forms(c))
	}
	if candidateByForm(c, "Leafs Crossing") == nil {
		t.Errorf("a plain-space phrase must still be captured; forms=%v", forms(c))
	}
}

func TestExtractCandidatesLowercasePhraseNotAcrossComma(t *testing.T) {
	work := t.TempDir()
	// "Night Blades" is an included capitalized phrase, but a comma-split lowercase
	// window "night, blades" must NOT match as the lowercase phrase "night blades".
	writeChapter(t, work, transcript.TextDir, 1,
		"The Night Blades struck at dusk. He feared the Night Blades. But at night, blades of grass swayed and night, blades glinted.")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	if candidateByForm(c, "Night Blades") == nil {
		t.Fatalf("Night Blades phrase should be included; forms=%v", forms(c))
	}
	if candidateByForm(c, "night blades") != nil {
		t.Errorf("a comma-split lowercase window must not match the phrase %q; forms=%v", "night blades", forms(c))
	}
}

func TestExtractCandidatesSnippetPreservesPunctuation(t *testing.T) {
	work := t.TempDir()
	// The snippet is a slice of the ORIGINAL text, so a comma beside the candidate
	// survives into the snippet (the old token-join would have dropped it, hiding that
	// the neighbours are a comma-separated list rather than a phrase).
	writeChapter(t, work, transcript.TextDir, 1,
		"The garden held Nightshade, Deathflower, and Shadowbane blooming. Again the Nightshade, Deathflower, and Shadowbane returned.")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	sb := candidateByForm(c, "Shadowbane")
	if sb == nil {
		t.Fatalf("Shadowbane should be a candidate; forms=%v", forms(c))
	}
	if len(sb.Snippets) == 0 {
		t.Fatalf("Shadowbane should have at least one snippet")
	}
	if !strings.Contains(sb.Snippets[0].Text, ",") {
		t.Errorf("snippet must preserve the source comma; got %q", sb.Snippets[0].Text)
	}
	if !strings.Contains(sb.Snippets[0].Text, "Shadowbane") {
		t.Errorf("snippet should contain the candidate form; got %q", sb.Snippets[0].Text)
	}
}

func TestExtractCandidatesRareLowercaseNonDictionary(t *testing.T) {
	work := t.TempDir()
	// "zephyrix" is not a common word and repeats 3 times, never capitalized.
	writeChapter(t, work, transcript.TextDir, 1,
		"the zephyrix hummed and the zephyrix glowed while the zephyrix pulsed with light")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	z := candidateByForm(c, "zephyrix")
	if z == nil {
		t.Fatalf("rare repeated non-dictionary lowercase token should be captured; forms=%v", forms(c))
	}
	if z.Count < 3 {
		t.Errorf("zephyrix count = %d, want >= 3", z.Count)
	}
	// A common lowercase word repeated must NOT be captured.
	if candidateByForm(c, "the") != nil || candidateByForm(c, "and") != nil {
		t.Errorf("common words must be filtered from the rare-lowercase heuristic; forms=%v", forms(c))
	}
}

func TestExtractCandidatesRepairedLayerPreference(t *testing.T) {
	work := t.TempDir()
	// Base text names "Xylophora"; the repaired copy replaces it with "Zylophora".
	// The extractor must read the repaired layer, so only Zylophora surfaces.
	writeChapter(t, work, transcript.TextDir, 1,
		"the Xylophora rose and the Xylophora fell near the Xylophora gate")
	writeChapter(t, work, transcript.RepairedDir, 1,
		"the Zylophora rose and the Zylophora fell near the Zylophora gate")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	if candidateByForm(c, "Zylophora") == nil {
		t.Fatalf("repaired-layer token Zylophora should be captured; forms=%v", forms(c))
	}
	if candidateByForm(c, "Xylophora") != nil {
		t.Errorf("base-text token must not appear when a repaired copy exists; forms=%v", forms(c))
	}
}

func TestExtractCandidatesSnippetCapAndDistinctChapters(t *testing.T) {
	// A candidate spanning 3 chapters gets exactly maxSnippets (2) snippets, one per
	// distinct chapter; a candidate confined to ONE chapter gets exactly 1 (no
	// same-chapter backfill). This pins the maxSnippets=2, one-per-chapter contract.
	work := t.TempDir()
	// "Kaelith" appears non-initially in chapters 1..3 (spans 3 chapters).
	for ch := 1; ch <= 3; ch++ {
		writeChapter(t, work, transcript.TextDir, ch,
			"the warrior Kaelith fought bravely and the warrior Kaelith stood firm again")
	}
	// "Brannoc" appears repeatedly but ONLY in chapter 4 (confined to one chapter).
	writeChapter(t, work, transcript.TextDir, 4,
		"the smith Brannoc worked and the smith Brannoc rested and the smith Brannoc sang")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}

	k := candidateByForm(c, "Kaelith")
	if k == nil {
		t.Fatalf("Kaelith should be a candidate; forms=%v", forms(c))
	}
	if len(k.Snippets) != maxSnippets {
		t.Errorf("Kaelith spans 3 chapters: snippets = %d, want exactly %d", len(k.Snippets), maxSnippets)
	}
	seen := map[int]bool{}
	for _, s := range k.Snippets {
		if seen[s.Chapter] {
			t.Errorf("snippet chapter %d repeated; want one per distinct chapter", s.Chapter)
		}
		seen[s.Chapter] = true
		if !strings.Contains(s.Text, "Kaelith") {
			t.Errorf("snippet %q should contain the form", s.Text)
		}
	}

	b := candidateByForm(c, "Brannoc")
	if b == nil {
		t.Fatalf("Brannoc should be a candidate; forms=%v", forms(c))
	}
	if len(b.Snippets) != 1 {
		t.Errorf("Brannoc is confined to one chapter: snippets = %d, want exactly 1 (no backfill)", len(b.Snippets))
	}
}

func TestExtractCandidatesDeterministic(t *testing.T) {
	work := t.TempDir()
	writeChapter(t, work, transcript.TextDir, 1,
		"The Night Blades rode to Leafs Crossing. Kael feared the Blades and the zephyrix glowed thrice, zephyrix, zephyrix.")
	writeChapter(t, work, transcript.TextDir, 2,
		"At Leafs Crossing the Night Blades waited. Kael watched d'Aston and d'Aston watched Kael in turn.")

	a, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	ja, err := MarshalCandidates(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := MarshalCandidates(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ja, jb) {
		t.Errorf("two runs produced different JSON:\n--- a ---\n%s\n--- b ---\n%s", ja, jb)
	}
}

func TestExtractCandidatesTruncationCounter(t *testing.T) {
	work := t.TempDir()
	// Generate more distinct single-token candidates than the single bucket cap so
	// it fires and records the drop count.
	var b strings.Builder
	total := maxSingleCandidates + 40
	for i := range total {
		// Distinct letter-only names (digits are token boundaries), each occurring
		// twice non-initially so it clears the single-token inclusion floor.
		name := "Zname" + letterSuffix(i)
		// Filler words are all common, so the only candidates are the names.
		fmt.Fprintf(&b, "the %s ran and the %s ran again. ", name, name)
	}
	writeChapter(t, work, transcript.TextDir, 1, b.String())

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Candidates) != maxSingleCandidates {
		t.Errorf("emitted %d candidates, want the single-bucket cap %d", len(c.Candidates), maxSingleCandidates)
	}
	if c.Truncated != total-maxSingleCandidates {
		t.Errorf("truncated = %d, want %d", c.Truncated, total-maxSingleCandidates)
	}
}

func TestExtractCandidatesHonorificInitialNovel(t *testing.T) {
	work := t.TempDir()
	// "Ashford" is always preceded by the honorific "Mr." - the period falsely marks it
	// sentence-initial, so its non_initial is 0. As a novel (non-common) word occurring
	// 3 times it must still be included (the fix-2 OR-branch).
	writeChapter(t, work, transcript.TextDir, 1,
		"Mr. Ashford arrived. Mr. Ashford waited. Mr. Ashford departed.")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	a := candidateByForm(c, "Ashford")
	if a == nil {
		t.Fatalf("Ashford should be included via the novel-initial branch; forms=%v", forms(c))
	}
	if a.NonInitial != 0 {
		t.Errorf("Ashford non_initial = %d, want 0 (every occurrence follows the honorific period)", a.NonInitial)
	}
	if a.Count < 3 {
		t.Errorf("Ashford count = %d, want >= 3", a.Count)
	}
}

func TestExtractCandidatesUnicodeName(t *testing.T) {
	work := t.TempDir()
	// A multi-byte name must surface as its EXACT form, not shatter into ASCII garbage.
	// "Renée" occurs 3 times non-initially.
	writeChapter(t, work, transcript.TextDir, 1,
		"the dancer Renée bowed and the dancer Renée spun while the dancer Renée laughed")

	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	r := candidateByForm(c, "Renée")
	if r == nil {
		t.Fatalf("Renée should surface as the exact multi-byte form; forms=%v", forms(c))
	}
	if r.Count < 3 {
		t.Errorf("Renée count = %d, want >= 3", r.Count)
	}
	// It must not fragment into a garbage token (e.g. a bare "Ren").
	if candidateByForm(c, "Ren") != nil {
		t.Errorf("Renée must not fragment into a bare Ren token; forms=%v", forms(c))
	}
}

func TestExtractCandidatesGeneratedFromHonest(t *testing.T) {
	// Text layer only -> the report names only transcripts-text.
	work := t.TempDir()
	writeChapter(t, work, transcript.TextDir, 1,
		"the warrior Kael fought and the warrior Kael won near the Kael gate")
	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	if c.GeneratedFrom != transcript.TextDir {
		t.Errorf("generated_from = %q, want %q (no repaired layer was read)", c.GeneratedFrom, transcript.TextDir)
	}

	// A repaired copy for one chapter -> the report names both layers.
	work2 := t.TempDir()
	writeChapter(t, work2, transcript.TextDir, 1, "the base Xylophora text here again Xylophora and Xylophora")
	writeChapter(t, work2, transcript.RepairedDir, 1, "the repaired Zylophora text here again Zylophora and Zylophora")
	c2, err := ExtractCandidates(work2)
	if err != nil {
		t.Fatal(err)
	}
	want := transcript.TextDir + " + " + transcript.RepairedDir
	if c2.GeneratedFrom != want {
		t.Errorf("generated_from = %q, want %q (a repaired chapter was read)", c2.GeneratedFrom, want)
	}
}

func TestExtractCandidatesTotalWords(t *testing.T) {
	work := t.TempDir()
	writeChapter(t, work, transcript.TextDir, 1, "one two three four five")
	writeChapter(t, work, transcript.TextDir, 2, "six seven eight")
	c, err := ExtractCandidates(work)
	if err != nil {
		t.Fatal(err)
	}
	if c.TotalWords != 8 {
		t.Errorf("total_words = %d, want 8 (5 + 3 tokens)", c.TotalWords)
	}
}

func TestDeadRules(t *testing.T) {
	work := t.TempDir()
	// The source names "Selene" (a form the ASR produced); it does NOT contain "Nomatch".
	writeChapter(t, work, transcript.TextDir, 1, "the ranger Selene aimed and Selene loosed the arrow")

	corr := &Corrections{Rules: []Rule{
		{Pattern: `(?<![A-Za-z])Selene(?![A-Za-z])`, Replacement: "Celaine", Note: "live rule"},
		{Pattern: `(?<![A-Za-z])Nomatch(?![A-Za-z])`, Replacement: "Whatever", Note: "dead rule"},
	}}
	dead, err := DeadRules(work, corr)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 {
		t.Fatalf("dead rules = %d, want 1; got %+v", len(dead), dead)
	}
	if dead[0].Pattern != `(?<![A-Za-z])Nomatch(?![A-Za-z])` {
		t.Errorf("dead rule = %q, want the Nomatch pattern", dead[0].Pattern)
	}
}

func TestDeadRulesOrderShadowedNotDead(t *testing.T) {
	work := t.TempDir()
	// The source contains "Foo Bar" and no bare "Foo" outside it. A whole-phrase rule
	// (rule 1) rewrites "Foo Bar" -> "Baz Bar", which in EVOLVING text would leave the
	// later bare-"Foo" rule (rule 2) with nothing to hit. But DeadRules matches the
	// ORIGINAL layer, where "Foo" genuinely occurs, so rule 2 is NOT dead.
	writeChapter(t, work, transcript.TextDir, 1, "they met at Foo Bar and left Foo Bar by dusk")

	corr := &Corrections{Rules: []Rule{
		{Pattern: `Foo Bar`, Replacement: "Baz Bar", Note: "whole phrase first"},
		{Pattern: `(?<![A-Za-z])Foo(?![A-Za-z])`, Replacement: "Baz", Note: "bare name second"},
	}}
	dead, err := DeadRules(work, corr)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 0 {
		t.Errorf("order-shadowed rules must NOT be flagged dead (they occur in the original layer); got %+v", dead)
	}
}

// TestExtractCandidatesRealData is an env-gated acceptance check against a real work
// dir (read-only). Run with:
//
//	AUDIOSILO_SIDECARS_CANDIDATES_DIR=/path/to/work go test ./internal/spelling/ \
//	  -run TestExtractCandidatesRealData -v
func TestExtractCandidatesRealData(t *testing.T) {
	dir := os.Getenv("AUDIOSILO_SIDECARS_CANDIDATES_DIR")
	if dir == "" {
		t.Skip("set AUDIOSILO_SIDECARS_CANDIDATES_DIR to run the real-data acceptance check")
	}
	c, err := ExtractCandidates(dir)
	if err != nil {
		t.Fatal(err)
	}
	j, err := MarshalCandidates(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("candidates: %d (truncated %d), json bytes: %d", len(c.Candidates), c.Truncated, len(j))
	for _, want := range []string{"Leafs Crossing", "Leaf's Crossing", "Night Blades", "night blades", "Seed Corps", "San Ren"} {
		if candidateByForm(c, want) != nil {
			t.Logf("  present: %q", want)
		} else {
			t.Logf("  MISSING: %q", want)
		}
	}
}
