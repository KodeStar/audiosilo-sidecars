package pipeline

import (
	"github.com/kodestar/audiosilo-sidecars/internal/ebook"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// chapterTextDir names the work-dir layer holding a book's FINAL per-chapter text -
// the prose the fact pass reads and the n-gram check measures the sidecars against.
//
// The two kinds keep separate directories rather than sharing one on purpose. An
// audio book's transcripts-corrected/ is DURABLE: it is the n-gram source for later
// re-validation and the corpus a sequel's spelling carryover copies from. An ebook's
// text is the copyrighted source prose itself, cheap to regenerate and required to be
// purged, so writing it into the same directory would either strand it or make the
// purge unsafe for audio.
func chapterTextDir(book store.Book) string {
	if state.ParseKind(book.Kind) == state.KindEbook {
		return ebook.TextDir
	}
	return spelling.CorrectedDir
}

// chunkPlanAuthor names the stage that should have written chunk_plan.json, so a
// missing plan points at the right stage for the book's kind.
func chunkPlanAuthor(book store.Book) string {
	if state.ParseKind(book.Kind) == state.KindEbook {
		return string(state.Extracting)
	}
	return string(state.SpellingResearch)
}
