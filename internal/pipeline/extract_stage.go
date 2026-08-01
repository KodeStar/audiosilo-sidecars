package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/extract"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/ebook"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// EbookUnreadableMsg is the needs_attention reason the extracting stage parks with
// when the epub cannot be opened at all. Human-fixable (supply a DRM-free copy, or
// enqueue the audiobook instead), so it parks - which Retry re-admits - rather than
// failing the book outright.
const EbookUnreadableMsg = "the epub could not be read: it is not a valid epub, is missing content it references, or is DRM-protected. " +
	"Supply a DRM-free copy, then Retry."

// extract is the ebook front half's only mechanical stage: it splits the epub into
// per-chapter text and decides the logical chapter universe those chapters will be
// published against.
//
// It is where the audio pipeline's inspect + split + asr + sanitize + qa + spelling
// are replaced by a zip read, because an epub's text is already exact.
func (e *Executor) extract(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	// A routing regression must never hand an audiobook folder to the epub reader:
	// the state machine picks the front half from books.kind, so a mismatch here means
	// the two disagree and the book would fail deep inside a zip parse.
	if state.ParseKind(book.Kind) != state.KindEbook || book.EbookPath == "" {
		return scheduler.StageResult{}, fmt.Errorf(
			"extracting: book %d is kind %q with ebook_path %q; only an ebook book can be extracted",
			book.ID, book.Kind, book.EbookPath)
	}
	if err := ctx.Err(); err != nil {
		return scheduler.StageResult{}, err
	}
	start := time.Now()
	splitDir := filepath.Join(book.WorkDir, ebook.ExtractDir)

	man, err := extract.Split(book.EbookPath, splitDir)
	if err != nil {
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkEbookUnreadable,
			fmt.Sprintf("%s (%v)", EbookUnreadableMsg, err))
	}
	u := ebook.BuildUniverse(man)
	// Record each section's opening words BEFORE writing the manifest: chapter_mapping
	// reads them to tell unlabelled front matter from another book's first page.
	ebook.PopulateHeads(&u, splitDir)
	if err := ebook.WriteManifest(book.WorkDir, u); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("extracting: write manifest: %w", err)
	}

	// Nothing numbered AND nothing an agent could map from: the file itself is the
	// problem, so a mapping round would only burn tokens confirming it.
	if len(u.Chapters) == 0 && u.Labeled < 2 {
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkEbookNoChapters,
			fmt.Sprintf("the epub's table of contents names no chapters this build can use (%d sections, %d labelled). "+
				"It may be a single unstructured document. Nothing can be mapped from it automatically.",
				len(u.Docs), u.Labeled))
	}

	notes(r, u, man)

	// Only a contiguous universe may be published against. When it is not, the stage
	// still succeeds - it wrote the draft manifest - and routes to chapter_mapping,
	// which corrects it exactly as markers_normalizing does for audio.
	if u.Contiguous {
		if err := e.materializeEbookChapters(ctx, book, u, splitDir, man); err != nil {
			return scheduler.StageResult{}, err
		}
	}

	result := scheduler.StageResult{
		ChaptersMapped: u.Contiguous,
		Metrics: metrics(map[string]any{
			"sections":          len(u.Docs),
			"chapters":          len(u.Chapters),
			"labelled":          u.Labeled,
			"words":             u.Words,
			"contiguous":        u.Contiguous,
			"suspected_excerpt": u.Suspected,
		}),
		RateSample: rateSample(1, time.Since(start).Seconds()),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Extracting), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// materializeEbookChapters writes everything the authoring tail reads: the chapter
// text, the shared chapter-universe manifest, and the fact-pass chunk plan.
//
// The chunk plan is computed HERE rather than in spelling_research, its author on
// the audio path, because that stage never runs for an ebook - there are no ASR
// misspellings to research.
func (e *Executor) materializeEbookChapters(ctx context.Context, book store.Book, u ebook.Universe, splitDir string, man *extract.Manifest) error {
	if err := ebook.WriteChapterText(book.WorkDir, splitDir, u); err != nil {
		return fmt.Errorf("extracting: write chapter text: %w", err)
	}
	if err := writeManifestJSON(book.WorkDir, ebookManifest(book, u, man)); err != nil {
		return fmt.Errorf("extracting: write manifest: %w", err)
	}
	chs := make([]chapterWords, 0, len(u.Chapters))
	for _, c := range u.Chapters {
		chs = append(chs, chapterWords{Chapter: c.Chapter, Words: c.Words})
	}
	if err := writeChunkPlan(book.WorkDir, planChunks(chs)); err != nil {
		return fmt.Errorf("extracting: write chunk plan: %w", err)
	}
	// Best-effort bookkeeping for the ETA engine and the Running list, exactly as
	// inspect does; a failure here never fails the stage.
	if e.db != nil {
		noCancel := context.WithoutCancel(ctx)
		_ = e.db.SetBookChapters(noCancel, book.ID, len(u.Chapters))
		_ = e.db.SetBookWords(noCancel, book.ID, u.Words)
	}
	return nil
}

// ebookManifest projects the chapter universe onto the manifest shape the authoring
// tail already reads.
//
// The manifest is the shared chapter-universe contract, not an audio artifact:
// classifyBookEdges, the chunk planner and the sidecar stages all consult it
// whatever the source was. An ebook's chapters carry Words instead of a Duration,
// and StyleEbook tells those readers not to reason about time.
func ebookManifest(book store.Book, u ebook.Universe, man *extract.Manifest) audio.Manifest {
	title := book.Title
	if title == "" && man != nil {
		title = man.Title
	}
	out := audio.Manifest{
		Source:       book.EbookPath,
		Title:        title,
		Style:        audio.StyleEbook,
		ChapterCount: len(u.Chapters),
	}
	for _, c := range u.Chapters {
		out.Chapters = append(out.Chapters, audio.Chapter{
			Chapter: c.Chapter,
			Title:   c.Title,
			Words:   c.Words,
		})
	}
	return out
}

// writeManifestJSON writes the shared manifest.json for a source that has no audio
// to probe.
func writeManifestJSON(workDir string, m audio.Manifest) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, audio.ManifestName), append(out, '\n'), 0o644)
}

// notes surfaces the extract verdict on the book's durable log: what was excluded,
// and - loudly - anything that looks like another book's text.
func notes(r scheduler.StageReport, u ebook.Universe, man *extract.Manifest) {
	if r.Note == nil {
		return
	}
	excluded := 0
	for _, d := range u.Docs {
		if d.Quarantine != "" {
			excluded++
		}
	}
	r.Note(fmt.Sprintf("extracted %d chapters from %d sections (%d excluded as front/back matter), %d words",
		len(u.Chapters), len(u.Docs), excluded, u.Words))
	for _, n := range u.Notes {
		r.Note(n)
	}
	if man != nil {
		for _, w := range man.Warnings {
			r.Note("epub: " + w)
		}
	}
	if !u.Contiguous {
		r.Note(fmt.Sprintf("the %d chapter numbers read from the table of contents do not form a contiguous run, "+
			"so chapter mapping will derive them", len(u.Chapters)))
	}
}
