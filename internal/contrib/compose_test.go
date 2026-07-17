package contrib

import (
	"strings"
	"testing"
)

// fence is the markdown code-fence marker (a Go raw string cannot contain a
// backtick, so it is built here).
const fence = "```"

func TestCharactersIssueGolden(t *testing.T) {
	payload := `{"work":"a-deadly-education","characters":[{"id":"el","name":"El","reveal":{"chapter":1},"description":"A sardonic student."}],"license":"CC-BY-SA-3.0","sources":[{"type":"community"}]}`
	title, body, labels := CharactersIssue("a-deadly-education", []byte(payload), "")

	if title != "[characters] a-deadly-education" {
		t.Fatalf("title = %q", title)
	}
	if strings.Join(labels, ",") != "data,data:characters" {
		t.Fatalf("labels = %v", labels)
	}

	want := "### The work\n\na-deadly-education\n\n" +
		"### The characters.json file\n\n" + fence + "json\n" + payload + "\n" + fence + "\n\n" +
		"### Own words\n\n- [x] Every description is my own words - no verbatim or near-verbatim text from the book, its jacket copy, or any wiki.\n\n" +
		"### Neutral voice\n\n- [x] The entries use a neutral reference-guide voice - no jokes, editorializing, profanity, or value judgements.\n\n" +
		"### License\n\n- [x] I license this contribution under CC BY-SA 3.0, and I have the right to do so.\n\n"
	if body != want {
		t.Fatalf("body mismatch:\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

func TestCharactersIssueGistLink(t *testing.T) {
	gist := "https://gist.githubusercontent.com/u/abc/raw/def/characters.json"
	_, body, _ := CharactersIssue("a-work", []byte(`{"work":"a-work"}`), gist)
	want := "### The characters.json file\n\n[characters.json](" + gist + ")\n\n"
	if !strings.Contains(body, want) {
		t.Fatalf("gist link section missing:\n%s", body)
	}
	if strings.Contains(body, fence) {
		t.Fatalf("gist mode must not inline a fenced payload:\n%s", body)
	}
}

func TestRecapsIssueGolden(t *testing.T) {
	payload := `{"work":"a-deadly-education","recaps":[{"through":{"chapter":1},"text":"So far, the school has been introduced."}],"license":"CC-BY-SA-3.0","sources":[{"type":"community"}]}`
	title, body, labels := RecapsIssue("a-deadly-education", []byte(payload), "")

	if title != "[recaps] a-deadly-education" {
		t.Fatalf("title = %q", title)
	}
	if strings.Join(labels, ",") != "data,data:recaps" {
		t.Fatalf("labels = %v", labels)
	}

	want := "### The work\n\na-deadly-education\n\n" +
		"### The recaps.json file\n\n" + fence + "json\n" + payload + "\n" + fence + "\n\n" +
		"### Own words\n\n- [x] Every recap is my own words - no verbatim or near-verbatim text from the book, its jacket copy, or any wiki - and stays within the length caps.\n\n" +
		"### Neutral voice\n\n- [x] The recaps use a neutral reference-guide voice - no jokes, editorializing, profanity, or value judgements - and the final entry states the actual ending.\n\n" +
		"### License\n\n- [x] I license this contribution under CC BY-SA 3.0, and I have the right to do so.\n\n"
	if body != want {
		t.Fatalf("body mismatch:\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

func TestWorkIssueGoldenFull(t *testing.T) {
	p := CoreProposal{
		Title:          "The Final Empire",
		Subtitle:       "Mistborn Book One",
		Authors:        []string{"Brandon Sanderson"},
		Language:       "en-US",
		FirstPublished: "2006",
		SeriesName:     "Mistborn",
		SeriesPosition: "1",
		PrintISBNs:     []string{"9780765311788"},
		Narrators:      []string{"Michael Kramer"},
		Abridged:       "Unabridged",
		RuntimeMin:     1500,
		ReleaseDate:    "2008-02-01",
		Publisher:      "Macmillan Audio",
		ASINs:          []RegionASIN{{Region: "US", ASIN: "B002UZMR2A"}, {Region: "GB", ASIN: "B00EXAMPLE1"}},
		AudiobookISBNs: []string{"9781427201591"},
		CoverURL:       "https://example.com/cover.jpg",
		Sources:        "Audible US product page (read 2026-07-17)",
	}
	title, body, labels := WorkIssue(p)
	if title != "[work] The Final Empire" {
		t.Fatalf("title = %q", title)
	}
	if strings.Join(labels, ",") != "data,data:add-work" {
		t.Fatalf("labels = %v", labels)
	}

	want := "### Title\n\nThe Final Empire\n\n" +
		"### Subtitle\n\nMistborn Book One\n\n" +
		"### Author(s)\n\nBrandon Sanderson\n\n" +
		"### Language\n\nen-US\n\n" +
		"### First published (year)\n\n2006\n\n" +
		"### Series name\n\nMistborn\n\n" +
		"### Series position\n\n1\n\n" +
		"### ISBN(s)\n\n9780765311788\n\n" +
		"### Narrator(s)\n\nMichael Kramer\n\n" +
		"### Abridged?\n\nUnabridged\n\n" +
		"### Runtime (minutes)\n\n1500\n\n" +
		"### Release date\n\n2008-02-01\n\n" +
		"### Publisher\n\nMacmillan Audio\n\n" +
		"### ASIN(s) with region\n\nUS: B002UZMR2A\nGB: B00EXAMPLE1\n\n" +
		"### Audiobook ISBN(s)\n\n9781427201591\n\n" +
		"### Cover image URL\n\nhttps://example.com/cover.jpg\n\n" +
		"### Sources\n\nAudible US product page (read 2026-07-17)\n\n" +
		"### Factual data\n\n- [x] This submission is factual data (no publisher blurb or copyrighted description).\n\n" +
		"### Public domain dedication\n\n- [x] I dedicate this contribution to the public domain under CC0-1.0, and I have the right to do so.\n\n"
	if body != want {
		t.Fatalf("body mismatch:\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

// TestWorkIssueGoldenMinimal: only required fields set - optional headings are
// omitted and Abridged? defaults to "Unknown".
func TestWorkIssueGoldenMinimal(t *testing.T) {
	p := CoreProposal{
		Title:     "Solo Book",
		Authors:   []string{"Cara Writer"},
		Language:  "en",
		Narrators: []string{"Dan Voice"},
		Sources:   "web",
	}
	_, body, _ := WorkIssue(p)
	want := "### Title\n\nSolo Book\n\n" +
		"### Author(s)\n\nCara Writer\n\n" +
		"### Language\n\nen\n\n" +
		"### Narrator(s)\n\nDan Voice\n\n" +
		"### Abridged?\n\nUnknown\n\n" +
		"### Sources\n\nweb\n\n" +
		"### Factual data\n\n- [x] This submission is factual data (no publisher blurb or copyrighted description).\n\n" +
		"### Public domain dedication\n\n- [x] I dedicate this contribution to the public domain under CC0-1.0, and I have the right to do so.\n\n"
	if body != want {
		t.Fatalf("body mismatch:\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
	// Guard: optional headings must be absent.
	for _, h := range []string{hSubtitle, hFirstPublished, hSeriesName, hISBN, hRuntime, hReleaseDate, hPublisher, hASINs, hAudiobookISBNs, hCoverURL} {
		if strings.Contains(body, "### "+h) {
			t.Fatalf("optional heading %q must be omitted when empty", h)
		}
	}
}

func TestCoreProposalValidate(t *testing.T) {
	valid := CoreProposal{
		Title:     "T",
		Authors:   []string{"A"},
		Language:  "en",
		Narrators: []string{"N"},
		Sources:   "S",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid proposal rejected: %v", err)
	}

	// Allowed: an ASIN carrying a region validates.
	withRegion := valid
	withRegion.ASINs = []RegionASIN{{Region: "us", ASIN: "B01"}}
	if err := withRegion.Validate(); err != nil {
		t.Fatalf("ASIN with a region rejected: %v", err)
	}
	// Allowed: a fully-empty ASIN entry is filtered at render, so it validates.
	withEmpty := valid
	withEmpty.ASINs = []RegionASIN{{}}
	if err := withEmpty.Validate(); err != nil {
		t.Fatalf("empty ASIN entry rejected: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*CoreProposal)
		wantSub string
	}{
		{"missing title", func(p *CoreProposal) { p.Title = "" }, "title"},
		{"blank title", func(p *CoreProposal) { p.Title = "   " }, "title"},
		{"missing authors", func(p *CoreProposal) { p.Authors = nil }, "author"},
		{"blank authors", func(p *CoreProposal) { p.Authors = []string{"  "} }, "author"},
		{"missing language", func(p *CoreProposal) { p.Language = "" }, "language"},
		{"missing narrators", func(p *CoreProposal) { p.Narrators = nil }, "narrator"},
		{"missing sources", func(p *CoreProposal) { p.Sources = "" }, "source"},
		{"asin without region", func(p *CoreProposal) { p.ASINs = []RegionASIN{{ASIN: "B01"}} }, "region"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			// Copy slices so the shared `valid` is not mutated across subtests.
			p.Authors = append([]string(nil), valid.Authors...)
			p.Narrators = append([]string(nil), valid.Narrators...)
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q should name the field (%q)", err, tc.wantSub)
			}
		})
	}
}

func TestExceedsBodyLimit(t *testing.T) {
	if BodySizeLimit != 60000 {
		t.Fatalf("BodySizeLimit = %d, want 60000", BodySizeLimit)
	}
	if ExceedsBodyLimit(strings.Repeat("x", BodySizeLimit)) {
		t.Fatal("a body exactly at the limit must not exceed it")
	}
	if !ExceedsBodyLimit(strings.Repeat("x", BodySizeLimit+1)) {
		t.Fatal("a body one byte over the limit must exceed it")
	}
}
