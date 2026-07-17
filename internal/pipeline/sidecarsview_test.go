package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSidecar writes body to <workDir>/sidecars/<name>, creating the dir.
func writeSidecar(t *testing.T, workDir, name, body string) {
	t.Helper()
	dir := filepath.Join(workDir, sidecarsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const sampleCharacters = `{
  "work": "the-blade-itself",
  "characters": [
    {"id": "logen-ninefingers", "name": "Logen Ninefingers", "aliases": ["The Bloody-Nine"], "role": "protagonist", "reveal": {"chapter": 1}, "description": "A feared Northman warrior.", "xref": {"wikidata": "Q123"}},
    {"id": "glokta", "name": "Sand dan Glokta", "role": "supporting", "reveal": {"chapter": 2}, "description": "A crippled inquisitor."}
  ],
  "license": "CC-BY-SA-3.0",
  "sources": [{"type": "community"}]
}`

const sampleRecaps = `{
  "work": "the-blade-itself",
  "recaps": [
    {"through": {"chapter": 1}, "text": "Logen escapes the Shanka."},
    {"through": {"chapter": 3}, "scope": "book", "text": "Glokta interrogates a mercer."}
  ],
  "in_short": "The first book of the First Law trilogy.",
  "ending": "The pieces are set for war.",
  "license": "CC-BY-SA-3.0",
  "sources": [{"type": "community"}]
}`

const sampleRecapsNoSummary = `{
  "work": "the-blade-itself",
  "recaps": [
    {"through": {"chapter": 1}, "text": "Logen escapes the Shanka."}
  ],
  "license": "CC-BY-SA-3.0",
  "sources": [{"type": "community"}]
}`

func TestSidecarsViewBothFiles(t *testing.T) {
	dir := t.TempDir()
	writeSidecar(t, dir, charactersFileName, sampleCharacters)
	writeSidecar(t, dir, recapsFileName, sampleRecaps)

	view, err := SidecarsView(dir)
	if err != nil {
		t.Fatalf("SidecarsView: %v", err)
	}
	if view.Work != "the-blade-itself" {
		t.Errorf("work = %q, want the-blade-itself", view.Work)
	}
	if len(view.Characters) != 2 {
		t.Fatalf("characters len = %d, want 2", len(view.Characters))
	}
	if view.Characters[0].ID != "logen-ninefingers" || view.Characters[0].Reveal.Chapter != 1 {
		t.Errorf("character[0] = %+v", view.Characters[0])
	}
	if view.Characters[0].Xref == nil || view.Characters[0].Xref.Wikidata != "Q123" {
		t.Errorf("character[0].xref = %+v", view.Characters[0].Xref)
	}
	if len(view.Recaps) != 2 || view.Recaps[1].Through.Chapter != 3 || view.Recaps[1].Scope != "book" {
		t.Errorf("recaps = %+v", view.Recaps)
	}
	if view.RecapSummary == nil {
		t.Fatal("recap_summary is nil, want in_short/ending")
	}
	if view.RecapSummary.InShort == "" || view.RecapSummary.Ending == "" {
		t.Errorf("recap_summary = %+v", view.RecapSummary)
	}
}

func TestSidecarsViewOnlyCharacters(t *testing.T) {
	dir := t.TempDir()
	writeSidecar(t, dir, charactersFileName, sampleCharacters)

	view, err := SidecarsView(dir)
	if err != nil {
		t.Fatalf("SidecarsView: %v", err)
	}
	if view.Work != "the-blade-itself" {
		t.Errorf("work = %q", view.Work)
	}
	if len(view.Characters) != 2 {
		t.Errorf("characters len = %d, want 2", len(view.Characters))
	}
	if view.Recaps != nil {
		t.Errorf("recaps = %+v, want nil", view.Recaps)
	}
	if view.RecapSummary != nil {
		t.Errorf("recap_summary = %+v, want nil", view.RecapSummary)
	}
}

func TestSidecarsViewOnlyRecaps(t *testing.T) {
	dir := t.TempDir()
	writeSidecar(t, dir, recapsFileName, sampleRecapsNoSummary)

	view, err := SidecarsView(dir)
	if err != nil {
		t.Fatalf("SidecarsView: %v", err)
	}
	if view.Work != "the-blade-itself" {
		t.Errorf("work = %q (recaps should supply it when characters is absent)", view.Work)
	}
	if view.Characters != nil {
		t.Errorf("characters = %+v, want nil", view.Characters)
	}
	if len(view.Recaps) != 1 {
		t.Errorf("recaps len = %d, want 1", len(view.Recaps))
	}
	if view.RecapSummary != nil {
		t.Errorf("recap_summary = %+v, want nil (no in_short/ending)", view.RecapSummary)
	}
}

func TestSidecarsViewNeither(t *testing.T) {
	dir := t.TempDir()
	_, err := SidecarsView(dir)
	if !errors.Is(err, ErrNoSidecars) {
		t.Fatalf("err = %v, want ErrNoSidecars", err)
	}
}

func TestSidecarsViewMalformed(t *testing.T) {
	dir := t.TempDir()
	writeSidecar(t, dir, charactersFileName, `{"work": "x", "characters": [}`) // invalid JSON

	_, err := SidecarsView(dir)
	if err == nil {
		t.Fatal("want an error for malformed characters.json")
	}
	if errors.Is(err, ErrNoSidecars) {
		t.Fatalf("malformed file mapped to ErrNoSidecars, want a hard error: %v", err)
	}
}

func TestSidecarsViewJSONShape(t *testing.T) {
	dir := t.TempDir()
	writeSidecar(t, dir, recapsFileName, sampleRecaps)

	raw, err := SidecarsViewJSON(dir)
	if err != nil {
		t.Fatalf("SidecarsViewJSON: %v", err)
	}
	s := string(raw)
	// The file-level license/sources wrappers must be dropped and the summaries
	// flattened to recap_summary.
	for _, absent := range []string{`"license"`, `"sources"`} {
		if strings.Contains(s, absent) {
			t.Errorf("preview JSON should not contain %s: %s", absent, s)
		}
	}
	for _, present := range []string{`"recap_summary"`, `"in_short"`, `"ending"`} {
		if !strings.Contains(s, present) {
			t.Errorf("preview JSON missing %s: %s", present, s)
		}
	}
}
