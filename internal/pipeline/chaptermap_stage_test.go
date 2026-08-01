package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/ebook"
)

func writeMap(t *testing.T, dir, body string) string {
	t.Helper()
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, chaptersFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func threeFiles() map[string]bool {
	return map[string]bool{"001.txt": true, "002.txt": true, "003.txt": true}
}

// TestValidateChapterMapAccepts covers the shapes a good mapping takes: a
// multi-file chapter, a quarantined section, and out-of-order numbering that is
// still a complete 1..N run.
func TestValidateChapterMapAccepts(t *testing.T) {
	out := writeMap(t, t.TempDir(), `{
	  "chapters": [
	    {"chapter": 2, "title": "Second", "files": ["003.txt"]},
	    {"chapter": 1, "title": "First", "files": ["001.txt", "002.txt"]}
	  ],
	  "quarantine": []
	}`)
	if err := validateChapterMap(out, threeFiles()); err != nil {
		t.Errorf("valid map rejected: %v", err)
	}

	out2 := writeMap(t, t.TempDir(), `{
	  "chapters": [
	    {"chapter": 1, "title": "", "files": ["001.txt"]},
	    {"chapter": 2, "title": "", "files": ["002.txt"]}
	  ],
	  "quarantine": [{"file": "003.txt", "reason": "excerpt of another book"}]
	}`)
	if err := validateChapterMap(out2, threeFiles()); err != nil {
		t.Errorf("map with a quarantined section rejected: %v", err)
	}
}

// TestValidateChapterMapRejects is the denied half. Every case here produces a
// WRONG chapter number rather than an error if it slips through - and a wrong
// chapter number is a wrong spoiler position in a published sidecar, which nothing
// downstream re-derives or re-checks.
func TestValidateChapterMapRejects(t *testing.T) {
	cases := []struct {
		name, body, wantErr string
	}{
		{
			"invented filename",
			`{"chapters":[{"chapter":1,"title":"","files":["001.txt"]},{"chapter":2,"title":"","files":["999.txt"]}],"quarantine":[{"file":"002.txt","reason":"x"},{"file":"003.txt","reason":"x"}]}`,
			"not one of the split sections",
		},
		{
			"a section claimed twice",
			`{"chapters":[{"chapter":1,"title":"","files":["001.txt"]},{"chapter":2,"title":"","files":["001.txt","002.txt"]}],"quarantine":[{"file":"003.txt","reason":"x"}]}`,
			"exactly one",
		},
		{
			"mapped and quarantined at once",
			`{"chapters":[{"chapter":1,"title":"","files":["001.txt"]},{"chapter":2,"title":"","files":["002.txt"]}],"quarantine":[{"file":"002.txt","reason":"x"},{"file":"003.txt","reason":"x"}]}`,
			"exactly one",
		},
		{
			"a section neither mapped nor quarantined",
			`{"chapters":[{"chapter":1,"title":"","files":["001.txt"]},{"chapter":2,"title":"","files":["002.txt"]}],"quarantine":[]}`,
			"neither",
		},
		{
			"gap in the numbering",
			`{"chapters":[{"chapter":1,"title":"","files":["001.txt"]},{"chapter":3,"title":"","files":["002.txt"]}],"quarantine":[{"file":"003.txt","reason":"x"}]}`,
			"no gaps",
		},
		{
			"duplicate chapter number",
			`{"chapters":[{"chapter":1,"title":"","files":["001.txt"]},{"chapter":1,"title":"","files":["002.txt"]}],"quarantine":[{"file":"003.txt","reason":"x"}]}`,
			"no gaps",
		},
		{
			"a chapter with no files",
			`{"chapters":[{"chapter":1,"title":"","files":["001.txt"]},{"chapter":2,"title":"","files":[]}],"quarantine":[{"file":"002.txt","reason":"x"},{"file":"003.txt","reason":"x"}]}`,
			"lists no files",
		},
		{
			"one chapter is a decline in disguise",
			`{"chapters":[{"chapter":1,"title":"","files":["001.txt","002.txt","003.txt"]}],"quarantine":[]}`,
			"decline in disguise",
		},
		{
			"unknown field",
			`{"chapters":[{"chapter":1,"title":"","files":["001.txt"],"start":0}],"quarantine":[]}`,
			`unknown field "start"`,
		},
		{
			"trailing content",
			`{"chapters":[{"chapter":1,"title":"","files":["001.txt"]},{"chapter":2,"title":"","files":["002.txt"]}],"quarantine":[{"file":"003.txt","reason":"x"}]} {"extra":1}`,
			"trailing content",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := writeMap(t, t.TempDir(), c.body)
			err := validateChapterMap(out, threeFiles())
			if err == nil {
				t.Fatalf("accepted an invalid map (%s)", c.name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %q, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

// TestApplyChapterMapPreservesProvenance: folding the agent's map back onto the
// recorded sections must keep each one's label, size and spine origin, so the
// persisted manifest still explains where every chapter came from.
func TestApplyChapterMapPreservesProvenance(t *testing.T) {
	draft := ebook.Universe{
		Words: 900,
		Docs: []ebook.Doc{
			{Index: 1, File: "001.txt", Spine: 1, Label: "Cover", Words: 10},
			{Index: 2, File: "002.txt", Spine: 2, Label: "I A Beginning", Words: 400},
			{Index: 3, File: "003.txt", Spine: 3, Label: "", Words: 300},
			{Index: 4, File: "004.txt", Spine: 4, Label: "II An Ending", Words: 190},
		},
	}
	var m agentChapterMap
	m.Chapters = append(m.Chapters,
		struct {
			Chapter int      `json:"chapter"`
			Title   string   `json:"title"`
			Files   []string `json:"files"`
		}{1, "A Beginning", []string{"002.txt", "003.txt"}},
		struct {
			Chapter int      `json:"chapter"`
			Title   string   `json:"title"`
			Files   []string `json:"files"`
		}{2, "An Ending", []string{"004.txt"}},
	)
	m.Quarantine = append(m.Quarantine, struct {
		File   string `json:"file"`
		Reason string `json:"reason"`
	}{"001.txt", "front matter: cover"})

	got := applyChapterMap(draft, m)

	if !got.Contiguous {
		t.Error("Contiguous = false; the validator accepts nothing but a complete run")
	}
	if len(got.Chapters) != 2 {
		t.Fatalf("chapters = %d, want 2", len(got.Chapters))
	}
	if len(got.Chapters[0].Files) != 2 || got.Chapters[0].Words != 700 {
		t.Errorf("chapter 1 = %+v, want both files and their combined words", got.Chapters[0])
	}
	if got.Chapters[0].Title != "A Beginning" {
		t.Errorf("chapter 1 title = %q, want the agent's title", got.Chapters[0].Title)
	}
	// Provenance survives.
	for _, d := range got.Docs {
		if d.File == "002.txt" {
			if d.Label != "I A Beginning" || d.Spine != 2 || d.Words != 400 {
				t.Errorf("section provenance lost: %+v", d)
			}
			if d.Source != ebook.SourceAgent {
				t.Errorf("source = %q, want %q so the manifest records who decided", d.Source, ebook.SourceAgent)
			}
		}
		if d.File == "001.txt" && d.Quarantine == "" {
			t.Error("the quarantined section lost its reason")
		}
	}
}

// TestReparseManifestRecoversAfterAVocabularyUpgrade: a book parked because an
// older parser could not read its numbering must be recoverable for free by a
// later one, with no agent round - the same upgrade path the audio marker parser
// gets. Section provenance, including the recorded head, survives.
func TestReparseManifestRecoversAfterAVocabularyUpgrade(t *testing.T) {
	work := t.TempDir()
	// A draft as an older parser would have left it: sections recorded, nothing
	// numbered, so not contiguous.
	stale := ebook.Universe{
		Contiguous: true, // deliberately wrong, to prove the reparse recomputes it
		Docs: []ebook.Doc{
			{Index: 1, File: "001.txt", Spine: 1, Label: "Chapter 1", Words: 900, Head: "the first head"},
			{Index: 2, File: "002.txt", Spine: 2, Label: "Chapter 2", Words: 900, Head: "the second head"},
			{Index: 3, File: "003.txt", Spine: 3, Label: "Chapter 3", Words: 900, Head: "the third head"},
		},
	}
	if err := ebook.WriteManifest(work, stale); err != nil {
		t.Fatal(err)
	}

	got, err := ebook.ReparseManifest(work)
	if err != nil {
		t.Fatalf("ReparseManifest: %v", err)
	}
	if !got.Contiguous || len(got.Chapters) != 3 {
		t.Fatalf("reparse gave %d chapters (contiguous %v), want 3", len(got.Chapters), got.Contiguous)
	}
	for _, d := range got.Docs {
		if d.Head == "" {
			t.Errorf("section %s lost its recorded head; the reparse does not re-read the split text", d.File)
		}
	}
}

// TestChapterMapResumesAHarvestedMapWithoutTheAgent: the stage writes the agent's
// map back to the extract manifest, THEN materializes, THEN writes the sentinel. A
// crash in that window leaves a contiguous draft, which skipped the reparse gate and
// sent the book back to the agent - paying a second time for a map already on disk,
// and risking a park if that round declined. A harvested map is recognizable by its
// agent-stamped sections.
func TestChapterMapResumesAHarvestedMapWithoutTheAgent(t *testing.T) {
	harvested := ebook.Universe{
		Contiguous: true,
		Chapters: []ebook.Chapter{
			{Chapter: 1, Files: []string{"002.txt"}, Words: 400},
			{Chapter: 2, Files: []string{"003.txt"}, Words: 300},
		},
		Docs: []ebook.Doc{
			{Index: 1, File: "001.txt", Spine: 1, Label: "Cover", Words: 10, Quarantine: "front matter"},
			{Index: 2, File: "002.txt", Spine: 2, Label: "I", Words: 400, Chapter: 1, Source: ebook.SourceAgent},
			{Index: 3, File: "003.txt", Spine: 3, Label: "II", Words: 300, Chapter: 2, Source: ebook.SourceAgent},
		},
	}
	if !agentMapped(harvested) {
		t.Fatal("agentMapped = false for a map the agent produced")
	}

	// A label-derived draft that merely came out contiguous must NOT match, or the
	// stage would skip an agent round it was never given.
	labelDerived := harvested
	labelDerived.Docs = append([]ebook.Doc(nil), harvested.Docs...)
	for i := range labelDerived.Docs {
		if labelDerived.Docs[i].Source == ebook.SourceAgent {
			labelDerived.Docs[i].Source = ebook.SourceStrict
		}
	}
	if agentMapped(labelDerived) {
		t.Error("agentMapped = true for a label-derived draft; only a harvested map may skip the agent")
	}
}
