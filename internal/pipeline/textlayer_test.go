package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/ebook"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// TestChapterTextDirPerKind pins the layer each kind's final prose lives in. They
// are separate because transcripts-corrected/ is DURABLE on the audio path (the
// n-gram source and a sequel's spelling-carryover corpus) while ebook text is the
// copyrighted source prose and must be purged - one directory could not be both.
func TestChapterTextDirPerKind(t *testing.T) {
	if got := chapterTextDir(store.Book{}); got != spelling.CorrectedDir {
		t.Errorf("default (pre-migration) book -> %q, want the audio layer %q", got, spelling.CorrectedDir)
	}
	if got := chapterTextDir(store.Book{Kind: "audio"}); got != spelling.CorrectedDir {
		t.Errorf("audio -> %q, want %q", got, spelling.CorrectedDir)
	}
	if got := chapterTextDir(store.Book{Kind: "ebook"}); got != ebook.TextDir {
		t.Errorf("ebook -> %q, want %q", got, ebook.TextDir)
	}
}

func TestChunkPlanAuthorPerKind(t *testing.T) {
	if got := chunkPlanAuthor(store.Book{}); got != string(state.SpellingResearch) {
		t.Errorf("audio chunk-plan author = %q, want %q", got, state.SpellingResearch)
	}
	if got := chunkPlanAuthor(store.Book{Kind: "ebook"}); got != string(state.Extracting) {
		t.Errorf("ebook chunk-plan author = %q, want %q", got, state.Extracting)
	}
}

// TestClassifyEbookEdgesExcludesNothing is the guard on the bug a shim would have
// introduced. The audio classifier drops an edge chapter that is short in BOTH words
// and duration; an ebook manifest has no durations, so that half always passes and a
// genuinely short opening chapter would be excluded - after which the assemble note
// tells the agent file N is spoken chapter N-1, shifting every reveal and recap one
// chapter earlier across the whole book.
func TestClassifyEbookEdgesExcludesNothing(t *testing.T) {
	work := t.TempDir()
	m := audio.Manifest{
		Style:        audio.StyleEbook,
		ChapterCount: 3,
		Chapters: []audio.Chapter{
			{Chapter: 1, Title: "A Very Short Opening", Words: 40}, // would trip the audio rule
			{Chapter: 2, Title: "The Middle", Words: 3000},
			{Chapter: 3, Title: "The End", Words: 2500},
		},
	}
	if err := audio.WriteManifest(work, m); err != nil {
		t.Fatal(err)
	}

	class, err := classifyBookEdges(work)
	if err != nil {
		t.Fatalf("classifyBookEdges: %v", err)
	}
	if class.LogicalCount != 3 {
		t.Errorf("LogicalCount = %d, want 3 - a short opening chapter is still a chapter", class.LogicalCount)
	}
	if len(class.ExcludedLeading) != 0 || len(class.ExcludedTrailing) != 0 {
		t.Errorf("exclusions = %v/%v, want none: extracting already quarantined non-story sections",
			class.ExcludedLeading, class.ExcludedTrailing)
	}
	// No renumbering happened, so the notes that explain one must stay empty.
	if class.ChunkNote != "" || class.AssembleNote != "" {
		t.Errorf("chunk/assemble notes = %q/%q, want empty: an ebook has no file-to-chapter offset",
			class.ChunkNote, class.AssembleNote)
	}
}

// TestNgramCheckFailsWithNothingToCheck: a check that measured nothing must not
// report a clean result. Skipping every layer silently would tell the auditor the
// sidecars carry no verbatim overlap when the source text was never compared, and
// nothing downstream re-runs the check.
func TestNgramCheckFailsWithNothingToCheck(t *testing.T) {
	work := t.TempDir()
	chars := filepath.Join(work, "characters.json")
	recaps := filepath.Join(work, "recaps.json")
	for _, p := range []string{chars, recaps} {
		if err := os.WriteFile(p, []byte(`{"characters":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ngramCheck(store.Book{Kind: "ebook", WorkDir: work}, chars, recaps); err == nil {
		t.Error("ngramCheck returned no error with no source layer; a vacuous pass must be loud")
	}
}

// TestNgramCheckUsesTheEbookLayer: with book text present the check runs against it
// and finds a planted copied run.
func TestNgramCheckUsesTheEbookLayer(t *testing.T) {
	work := t.TempDir()
	textDir := filepath.Join(work, ebook.TextDir)
	if err := os.MkdirAll(textDir, 0o750); err != nil {
		t.Fatal(err)
	}
	const copied = "the tall stranger walked into the quiet room and said nothing at all"
	if err := os.WriteFile(filepath.Join(textDir, "ch001.txt"), []byte(copied), 0o600); err != nil {
		t.Fatal(err)
	}
	chars := filepath.Join(work, "characters.json")
	recaps := filepath.Join(work, "recaps.json")
	if err := os.WriteFile(chars, []byte(`{"characters":[{"id":"x","name":"X","description":"`+copied+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recaps, []byte(`{"recaps":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	findings, err := ngramCheck(store.Book{Kind: "ebook", WorkDir: work}, chars, recaps)
	if err != nil {
		t.Fatalf("ngramCheck: %v", err)
	}
	if len(findings) == 0 {
		t.Error("no finding for a verbatim run copied straight out of the book text")
	}
}
