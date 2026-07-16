// Package pipeline wires the real stage executors into the scheduler. It provides
// a composite scheduler.Executor that routes each pipeline stage to its
// implementation - inspecting -> internal/audio.Inspect, splitting ->
// internal/audio.Split - and falls through to a stub for every stage a later
// milestone still owns (ASR, the agent stages, contribute). The scheduler API is
// unchanged: it sees one Executor.
//
// Each real stage writes its _done/<stage>.json sentinel as its final durable
// action (the crash-resume contract) and returns the branch decision the state
// machine consults (inspecting carries MarkersContiguous; splitting reports
// progress). The composite lives here, not in the scheduler, so the scheduler
// stays free of audio/tool concerns; server.go constructs it with the resolved
// ffmpeg/ffprobe paths.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/scratch"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// Executor is the composite stage executor. ffmpeg/ffprobe are the resolved tool
// paths ("" when unavailable, in which case the audio stages fail their book with
// a clear error while the rest of the daemon keeps working). fallback runs every
// stage the real executors don't yet implement. db is used to account a book's
// scratch size once split has written the chapter FLACs.
type Executor struct {
	db       *store.DB
	ffmpeg   string
	ffprobe  string
	fallback scheduler.Executor
}

// NewExecutor builds a composite executor over the resolved tool paths, delegating
// unimplemented stages to fallback (the stub executor in M2). db may be nil in
// tests that don't assert scratch accounting.
func NewExecutor(db *store.DB, ffmpeg, ffprobe string, fallback scheduler.Executor) *Executor {
	return &Executor{db: db, ffmpeg: ffmpeg, ffprobe: ffprobe, fallback: fallback}
}

// Execute routes a stage to its implementation. Inspecting and splitting are real
// in M2; everything else falls through to the stub.
func (e *Executor) Execute(ctx context.Context, book store.Book, stage state.State, report scheduler.ProgressFunc) (scheduler.StageResult, error) {
	switch stage {
	case state.Inspecting:
		return e.inspect(ctx, book)
	case state.MarkersNormalizing:
		// A book whose chapter markers are not a clean contiguous run (or that has no
		// usable markers at all) lands here. Automatic marker normalization is a later
		// milestone (M5), so rather than let the stub advance it into a split with no
		// manifest, park it needs_attention with a clear, human-facing reason.
		return scheduler.StageResult{}, scheduler.Park(MarkersNormalizingMsg)
	case state.Splitting:
		return e.split(ctx, book, report)
	default:
		return e.fallback.Execute(ctx, book, stage, report)
	}
}

// MarkersNormalizingMsg is the needs_attention reason a book is parked with when
// its chapter markers cannot be normalized automatically yet (M5). Exported so a
// test (and later the UI) can assert/label it exactly.
const MarkersNormalizingMsg = "chapter markers need manual mapping - automatic normalization arrives in a later milestone (M5)"

// inspect probes the book's source audio, writes probe.json + manifest.json, and
// records whether the chapter markers are contiguous (drives the
// markers_normalizing skip). It writes the stage sentinel as its final action.
func (e *Executor) inspect(ctx context.Context, book store.Book) (scheduler.StageResult, error) {
	manifest, contiguous, err := audio.Inspect(ctx, book.SourcePath, book.WorkDir, e.ffprobe)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("inspect: %w", err)
	}
	result := scheduler.StageResult{
		MarkersContiguous: contiguous,
		Metrics: metrics(map[string]any{
			"style":         manifest.Style,
			"chapter_count": manifest.ChapterCount,
			"duration_sec":  manifest.Duration,
			"contiguous":    contiguous,
		}),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Inspecting), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// split converts each manifest chapter into a mono/16 kHz FLAC, reporting progress
// per chapter, then writes the stage sentinel.
func (e *Executor) split(ctx context.Context, book store.Book, report scheduler.ProgressFunc) (scheduler.StageResult, error) {
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("split: read manifest (inspect must run first): %w", err)
	}
	if err := audio.Split(ctx, manifest, book.WorkDir, e.ffmpeg, func(done, total int) {
		if report != nil {
			report(done, total)
		}
	}); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("split: %w", err)
	}
	// Account the work dir's on-disk size now (one walk) so the book list and the
	// /system gauge serve scratch_bytes from the DB without walking on every read.
	// Use a non-cancellable context: split completed successfully, so a shutdown/
	// cancel racing this final gauge write must not silently drop it and leave the
	// column stale (the accounting is the last durable step, like closing the run).
	if e.db != nil {
		if n, derr := scratch.DirSize(book.WorkDir); derr == nil {
			_ = e.db.UpdateScratchBytes(context.WithoutCancel(ctx), book.ID, n)
		}
	}
	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{
			"style":         manifest.Style,
			"chapter_count": manifest.ChapterCount,
		}),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Splitting), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// metrics marshals a stage's metrics map, tolerating a marshal failure (metrics
// are advisory - a failure here must not fail the stage).
func metrics(m map[string]any) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}
