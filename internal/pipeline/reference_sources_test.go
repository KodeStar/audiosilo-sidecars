package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
)

// writeRefFixtures lays out a work dir the way the daemon does before the spelling
// agent runs: the publisher's marker titles and the series predecessor's ledger.
func writeRefFixtures(t *testing.T, markerTitles, priorLedger string) string {
	t.Helper()
	dir := t.TempDir()
	if markerTitles != "" {
		if err := os.WriteFile(filepath.Join(dir, markerTitlesFile), []byte(markerTitles), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if priorLedger != "" {
		if err := os.MkdirAll(filepath.Join(dir, spellingRefsDir), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, spellingRefsDir, priorSpellingsFile), []byte(priorLedger), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func sourceByName(srcs []spelling.ReferenceSource, name string) (spelling.ReferenceSource, bool) {
	for _, s := range srcs {
		if s.Name == name {
			return s, true
		}
	}
	return spelling.ReferenceSource{}, false
}

// TestReferenceSourcesRanksGlossaryAsVerifiedAndCarryoverAsCarryover pins the
// authority assignment. The glossary comes from outside the recording, so it may
// contradict it; the predecessor's ledger only says how the LAST book heard the
// name, which is how a mistake travels down a series.
func TestReferenceSourcesRanksGlossaryAsVerifiedAndCarryoverAsCarryover(t *testing.T) {
	dir := writeRefFixtures(t,
		"Chapter 9: Toren\n",
		`{"ledger":[{"canonical":"Torrin"},{"canonical":"Floss"},{"canonical":"  "}]}`)
	g := metaops.Glossary{
		SeriesName: "The Wandering Inn",
		Works:      []string{"w-book5"},
		Names:      []string{"Flos", "Toren"},
	}

	srcs := referenceSources(dir, g)

	glossary, ok := sourceByName(srcs, filepath.Join(spellingRefsDir, seriesGlossaryFile))
	if !ok {
		t.Fatal("the series glossary is missing from the reference sources")
	}
	if glossary.Authority != spelling.AuthorityVerified {
		t.Errorf("glossary authority = %q, want verified", glossary.Authority)
	}
	if len(glossary.Names) != 2 {
		t.Errorf("glossary names = %v", glossary.Names)
	}

	markers, ok := sourceByName(srcs, markerTitlesFile)
	if !ok {
		t.Fatal("marker titles are missing from the reference sources")
	}
	if markers.Authority != spelling.AuthorityVerified {
		t.Errorf("marker-title authority = %q, want verified", markers.Authority)
	}

	prior, ok := sourceByName(srcs, filepath.Join(spellingRefsDir, priorSpellingsFile))
	if !ok {
		t.Fatal("the carried ledger is missing from the reference sources")
	}
	if prior.Authority != spelling.AuthorityCarryover {
		t.Errorf("carryover authority = %q, want carryover", prior.Authority)
	}
	// A blank canonical is dropped rather than admitted as an empty vocabulary entry.
	if len(prior.Names) != 2 {
		t.Errorf("carried ledger names = %v, want the two non-blank canonicals", prior.Names)
	}

	// End to end: the glossary must win the proposal for a name the carryover
	// spells wrong, which is the whole point of the ranking.
	cands := &spelling.Candidates{Candidates: []spelling.Candidate{{Form: "Torrin", Count: 369}}}
	got := spelling.BuildReferenceMatches(cands, srcs)
	if len(got.Matches) != 1 {
		t.Fatalf("want one proposal, got %+v", got.Matches)
	}
	if got.Matches[0].Reference != "Toren" || got.Matches[0].Authority != spelling.AuthorityVerified {
		t.Errorf("proposal = %+v, want Toren from a verified source", got.Matches[0])
	}
}

// TestPopulateSpellingRefsIsNotBlockedByTheGlossary guards a real regression: the
// carryover copy used to skip on "spelling-refs/ is non-empty", but the glossary is
// a second, independent daemon-side writer to that same directory. A book whose
// first attempt ran before its series predecessor finished writes a glossary and no
// carryover; on the retry that finally HAS a predecessor, the directory-level guard
// would see the glossary and skip the carryover for good.
func TestPopulateSpellingRefsIsNotBlockedByTheGlossary(t *testing.T) {
	workDir := t.TempDir()
	predDir := t.TempDir()

	// The predecessor's ledger is what the carryover copy must deliver.
	if err := os.WriteFile(filepath.Join(predDir, spelling.SpellingsFile),
		[]byte(`{"ledger":[{"canonical":"Toren"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Attempt 1 wrote only a glossary (no predecessor was available yet).
	refs := filepath.Join(workDir, spellingRefsDir)
	if err := os.MkdirAll(refs, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refs, seriesGlossaryFile), []byte("Toren\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := populateSpellingRefs(workDir, predDir); err != nil {
		t.Fatalf("populateSpellingRefs: %v", err)
	}
	if !fsutil.IsFile(filepath.Join(refs, priorSpellingsFile)) {
		t.Fatal("the carryover was skipped because the glossary had already written to spelling-refs/")
	}
	// The glossary must survive the carryover copy.
	if !fsutil.IsFile(filepath.Join(refs, seriesGlossaryFile)) {
		t.Error("the carryover copy clobbered the glossary")
	}
}

// TestEvidencePriority pins the rendered evidence list for all four book shapes.
// Two of the seven tiers are conditional, so the numbering is the thing that rots;
// composing the list in Go is what lets one table hold every combination instead of
// four hand-numbered copies in the template.
func TestEvidencePriority(t *testing.T) {
	const (
		glossaryTier = "1. the community series glossary (`spelling-refs/series-glossary.txt`)"
		markerTier   = "embedded metadata and exact chapter-marker labels (`marker_titles.txt`)"
		ledgerTier   = "the carried series ledger (`spelling-refs/prior-spellings.json`)"
	)
	cases := []struct {
		name                     string
		glossary, carryover      bool
		wantLines                int
		wantFirst, wantLastFixed string
	}{
		{"neither", false, false, 5, "1. " + markerTier, "5. agreement among multiple independent references"},
		{"carryover only", false, true, 6, "1. " + markerTier, "6. agreement among multiple independent references"},
		{"glossary only", true, false, 6, glossaryTier, "6. agreement among multiple independent references"},
		{"both", true, true, 7, glossaryTier, "7. agreement among multiple independent references"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := strings.Split(evidencePriority(tc.glossary, tc.carryover), "\n")
			if len(lines) != tc.wantLines {
				t.Fatalf("got %d tiers, want %d:\n%s", len(lines), tc.wantLines, strings.Join(lines, "\n"))
			}
			for i, line := range lines {
				if !strings.HasPrefix(line, fmt.Sprintf("%d. ", i+1)) {
					t.Errorf("tier %d is misnumbered: %q", i+1, line)
				}
			}
			if lines[0] != tc.wantFirst {
				t.Errorf("first tier = %q, want %q", lines[0], tc.wantFirst)
			}
			if lines[len(lines)-1] != tc.wantLastFixed {
				t.Errorf("last tier = %q, want %q", lines[len(lines)-1], tc.wantLastFixed)
			}
			// The precedence rule: whenever both are present, the glossary must be
			// ranked above the carried ledger.
			if tc.glossary && tc.carryover {
				joined := strings.Join(lines, "\n")
				if strings.Index(joined, glossaryTier) > strings.Index(joined, ledgerTier) {
					t.Error("the glossary must outrank the carried ledger")
				}
			}
		})
	}
}

// TestReferenceSourcesToleratesMissingInputs: a first-in-series book has no
// carryover, a standalone work has no glossary, and a real Wandering Inn volume
// ships a bare-number marker table. Every combination must degrade to "fewer
// sources", never an error.
func TestReferenceSourcesToleratesMissingInputs(t *testing.T) {
	t.Run("nothing at all", func(t *testing.T) {
		if srcs := referenceSources(t.TempDir(), metaops.Glossary{}); len(srcs) != 0 {
			t.Errorf("want no sources, got %+v", srcs)
		}
	})
	t.Run("bare-number marker table contributes nothing", func(t *testing.T) {
		dir := writeRefFixtures(t, "001\n002\n003\n", "")
		if srcs := referenceSources(dir, metaops.Glossary{}); len(srcs) != 0 {
			t.Errorf("a numeric marker table names nobody; got %+v", srcs)
		}
	})
	t.Run("unparseable prior ledger is skipped", func(t *testing.T) {
		dir := writeRefFixtures(t, "", "not json")
		if srcs := referenceSources(dir, metaops.Glossary{}); len(srcs) != 0 {
			t.Errorf("want no sources, got %+v", srcs)
		}
	})
	t.Run("glossary alone is enough", func(t *testing.T) {
		g := metaops.Glossary{Names: []string{"Toren"}}
		srcs := referenceSources(t.TempDir(), g)
		if len(srcs) != 1 || srcs[0].Authority != spelling.AuthorityVerified {
			t.Errorf("want one verified source, got %+v", srcs)
		}
	})
}
