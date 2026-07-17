package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// baseChars/baseRecaps build a structurally valid sidecar pair (seriesless => a
// series opener), the fixture every violation test mutates from.
func baseChars(work string) *model.Characters {
	return &model.Characters{
		Work: work,
		Characters: []model.Character{{
			ID:          "alice",
			Name:        "Alice",
			Reveal:      model.Position{Chapter: 1},
			Description: "A knight of the northern realm.",
		}},
		License: sidecarLicenseContent,
		Sources: []model.Source{{Type: sourceTypeCommunity}},
	}
}

func baseRecaps(work string) *model.Recaps {
	return &model.Recaps{
		Work:    work,
		Recaps:  []model.Recap{{Through: model.Position{Chapter: 3}, Text: "The kingdom fell and rose again."}},
		License: sidecarLicenseContent,
		Sources: []model.Source{{Type: sourceTypeCommunity}},
	}
}

func TestValidateSidecars(t *testing.T) {
	const chapters = 3
	cases := []struct {
		name     string
		opener   bool
		mutate   func(c *model.Characters, r *model.Recaps)
		wantErr  string // substring expected among errs ("" => none)
		wantWarn string // substring expected among warns ("" => none)
	}{
		{name: "valid opener", opener: true},
		{name: "valid non-opener with series recap", opener: false, mutate: func(_ *model.Characters, r *model.Recaps) {
			r.Recaps = append([]model.Recap{{Through: model.Position{Chapter: 0}, Scope: "series", Text: "Previously in earlier books."}}, r.Recaps...)
		}},
		{name: "bad work slug", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.Work = "Not A Slug" }, wantErr: "kebab-case slug"},
		{name: "bad license", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.License = "CC0-1.0" }, wantErr: "license must be"},
		{name: "no sources", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.Sources = nil }, wantErr: "sources must be exactly"},
		{name: "wrong source type", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.Sources = []model.Source{{Type: "wiki"}} }, wantErr: "sources must be exactly"},
		{name: "no characters", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.Characters = nil }, wantErr: "at least one character"},
		{name: "bad id", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.Characters[0].ID = "Bad_ID" }, wantErr: "kebab-case slug"},
		{name: "duplicate id", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) {
			c.Characters = append(c.Characters, model.Character{ID: "alice", Name: "Alicia", Reveal: model.Position{Chapter: 2}, Description: "Another."})
		}, wantErr: "not unique"},
		{name: "empty name", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.Characters[0].Name = "" }, wantErr: "name is empty"},
		{name: "bad role", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.Characters[0].Role = "hero" }, wantErr: "is not one of"},
		{name: "reveal out of range", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.Characters[0].Reveal.Chapter = 9 }, wantErr: "outside the range"},
		{name: "empty description", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.Characters[0].Description = "" }, wantErr: "description is empty"},
		{name: "description over cap", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) {
			c.Characters[0].Description = strings.Repeat("a", capDescription+1)
		}, wantErr: "over the 1500 cap"},
		{name: "em dash in description", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) {
			c.Characters[0].Description = "A knight " + string(emDash) + " brave."
		}, wantErr: "em dash"},
		{name: "empty alias", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) { c.Characters[0].Aliases = []string{""} }, wantErr: "aliases[0] is empty"},
		{name: "bad wikidata", opener: true, mutate: func(c *model.Characters, _ *model.Recaps) {
			c.Characters[0].Xref = &model.CharacterXref{Wikidata: "wrong"}
		}, wantErr: "xref.wikidata"},
		{name: "recap out of range", opener: true, mutate: func(_ *model.Characters, r *model.Recaps) { r.Recaps[0].Through.Chapter = 12 }, wantErr: "outside the range"},
		{name: "duplicate through", opener: true, mutate: func(_ *model.Characters, r *model.Recaps) {
			r.Recaps = append(r.Recaps, model.Recap{Through: model.Position{Chapter: 3}, Text: "Again."})
		}, wantErr: "not unique"},
		{name: "empty text", opener: true, mutate: func(_ *model.Characters, r *model.Recaps) { r.Recaps[0].Text = "" }, wantErr: "text is empty"},
		{name: "text over cap", opener: true, mutate: func(_ *model.Characters, r *model.Recaps) { r.Recaps[0].Text = strings.Repeat("b", capRecapText+1) }, wantErr: "over the 3000 cap"},
		{name: "bad scope", opener: true, mutate: func(_ *model.Characters, r *model.Recaps) { r.Recaps[0].Scope = "chapter" }, wantErr: "not book/series"},
		{name: "in_short over cap", opener: true, mutate: func(_ *model.Characters, r *model.Recaps) { r.InShort = strings.Repeat("c", capInShort+1) }, wantErr: "in_short is"},
		{name: "ending em dash", opener: true, mutate: func(_ *model.Characters, r *model.Recaps) { r.Ending = "It ends " + string(emDash) + " here." }, wantErr: "ending contains an em dash"},
		{name: "opener must not have series recap", opener: true, mutate: func(_ *model.Characters, r *model.Recaps) {
			r.Recaps = append(r.Recaps, model.Recap{Through: model.Position{Chapter: 0}, Scope: "series", Text: "Prev."})
		}, wantErr: "must NOT carry a chapter:0"},
		{name: "non-opener missing series recap", opener: false, wantWarn: "should carry a chapter:0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, r := baseChars("the-kingdom"), baseRecaps("the-kingdom")
			if tc.mutate != nil {
				tc.mutate(c, r)
			}
			errs, warns := validateSidecars(c, r, chapters, tc.opener)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Errorf("unexpected errors: %v", errs)
				}
			} else if !containsSub(errs, tc.wantErr) {
				t.Errorf("errors %v do not contain %q", errs, tc.wantErr)
			}
			if tc.wantWarn != "" && !containsSub(warns, tc.wantWarn) {
				t.Errorf("warnings %v do not contain %q", warns, tc.wantWarn)
			}
		})
	}
}

func containsSub(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestVerifiedLedgerTable(t *testing.T) {
	work := t.TempDir()
	// No spellings.json -> empty ledger.
	got, err := verifiedLedgerTable(work)
	if err != nil {
		t.Fatalf("no-file: %v", err)
	}
	if got != "" {
		t.Errorf("no spellings.json should yield empty ledger, got %q", got)
	}

	writeSpellings(t, work, `{
  "title": "Book",
  "chunk_ends": [3],
  "ledger": [
    {"canonical": "Celaine", "type": "person", "status": "verified", "variants": "Selene, Seline"},
    {"canonical": "Bob", "type": "person", "status": "probable", "variants": "Bab"}
  ]
}`)
	got, err = verifiedLedgerTable(work)
	if err != nil {
		t.Fatalf("verifiedLedgerTable: %v", err)
	}
	if !strings.Contains(got, "Celaine") || !strings.Contains(got, "Selene, Seline") {
		t.Errorf("ledger table missing the verified entry: %q", got)
	}
	if strings.Contains(got, "Bob") {
		t.Errorf("ledger table leaked a non-verified (probable) entry: %q", got)
	}
	if !strings.Contains(got, "| canonical | type | aliases/variants |") {
		t.Errorf("ledger table missing header: %q", got)
	}
}

func TestVerifiedLedgerTableAllProbableIsEmpty(t *testing.T) {
	work := t.TempDir()
	writeSpellings(t, work, `{"title":"B","chunk_ends":[2],"ledger":[{"canonical":"Bob","type":"person","status":"probable"}]}`)
	got, err := verifiedLedgerTable(work)
	if err != nil {
		t.Fatalf("verifiedLedgerTable: %v", err)
	}
	if got != "" {
		t.Errorf("no verified entries should yield empty ledger, got %q", got)
	}
}

func TestWorkSlug(t *testing.T) {
	if got := workSlug(store.Book{WorkID: "the-final-empire"}); got != "the-final-empire" {
		t.Errorf("WorkID should win: got %q", got)
	}
	if got := workSlug(store.Book{Title: "The Way of Kings!"}); got != "the-way-of-kings" {
		t.Errorf("title slug = %q, want the-way-of-kings", got)
	}
	if got := workSlug(store.Book{Title: "***"}); got != "book" {
		t.Errorf("all-symbol title should fall back to book, got %q", got)
	}
	if !model.ValidSlug(workSlug(store.Book{Title: "A B C"})) {
		t.Error("slugified title is not a valid slug")
	}
}

func writeSpellings(t *testing.T, workDir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workDir, spelling.SpellingsFile), []byte(body), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}
