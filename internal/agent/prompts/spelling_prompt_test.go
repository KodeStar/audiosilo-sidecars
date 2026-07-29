package prompts

import (
	"strings"
	"testing"
)

// spellingData builds the spelling.md template data for one branch combination. The
// evidence-priority list is composed in Go (pipeline.evidencePriority) and injected,
// so this stands in for it; its ordering is tested there.
func spellingData(glossary, carryover, web bool) map[string]any {
	return map[string]any{
		"Title": "The General of Izril", "Authors": "pirateaba", "Series": "The Wandering Inn",
		"SeriesPos": "6", "ChunkEnds": "10,20",
		"HasCarryover": carryover, "WebAvailable": web, "HasGlossary": glossary,
		"GlossaryWorks": "w-book4, w-book5", "HasReferenceMatches": true,
		"EvidencePriority": "1. the community series glossary (`spelling-refs/series-glossary.txt`)\n" +
			"2. embedded metadata and exact chapter-marker labels (`marker_titles.txt`)\n" +
			"3. the carried series ledger (`spelling-refs/prior-spellings.json`)",
	}
}

// TestSpellingPromptRendersEveryBranch renders spelling.md across the
// glossary x carryover x web matrix. Each flag depends on the book (a standalone
// work has no glossary, book 1 has no carryover), so a broken branch would
// otherwise only surface on a live run of the one book that trips it.
func TestSpellingPromptRendersEveryBranch(t *testing.T) {
	for _, glossary := range []bool{false, true} {
		for _, carryover := range []bool{false, true} {
			for _, web := range []bool{false, true} {
				out, err := Render("spelling.md", spellingData(glossary, carryover, web))
				if err != nil {
					t.Fatalf("glossary=%v carryover=%v web=%v: %v", glossary, carryover, web, err)
				}
				if strings.Contains(out, "{{") || strings.Contains(out, "<no value>") {
					t.Errorf("glossary=%v carryover=%v web=%v: left an unrendered template action", glossary, carryover, web)
				}
				// The glossary input block and the reference_files allow-list must
				// both be gated on the same flag: a prompt that describes a file the
				// stage did not stage sends the agent looking for it, and one that
				// omits it from the allow-list makes citing it a validation failure.
				const inputBlock = "`spelling-refs/series-glossary.txt` - the canonical character names"
				const allowList = "`marker_titles.txt`, `spelling-refs/series-glossary.txt`"
				if strings.Contains(out, inputBlock) != glossary {
					t.Errorf("glossary=%v: input-block mention mismatch", glossary)
				}
				if strings.Contains(out, allowList) != glossary {
					t.Errorf("glossary=%v: reference_files allow-list mismatch", glossary)
				}
			}
		}
	}
}

// TestSpellingPromptRanksGlossaryAboveCarryover pins the precedence rule this
// feature turns on. The carried ledger legitimately outranks raw ASR, and the
// prompt says so - but a previous volume's mistake is carried in it verbatim, so it
// must NOT outrank a verified outside source. Getting this backwards is what let
// one misheard name propagate across an entire series.
func TestSpellingPromptRanksGlossaryAboveCarryover(t *testing.T) {
	out, err := Render("spelling.md", spellingData(true, true, true))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "does NOT win over the series glossary") {
		t.Error("the carryover section must state that it loses to the glossary")
	}
	// Transcript frequency must be explicitly excluded as evidence: 1135 identical
	// occurrences are one ASR decision, not corroboration.
	if !strings.Contains(out, "frequency is NOT on this list") {
		t.Error("the prompt must state that transcript frequency is not evidence of spelling")
	}
	// The report the pre-pass stages must be dispositioned, not silently ignored.
	if !strings.Contains(out, "spelling_reference_matches.json") {
		t.Error("the prompt must tell the agent to work through the reference-match report")
	}
}
