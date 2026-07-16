// Package pipeline wires the real stage executors into the scheduler. It provides
// a composite scheduler.Executor that routes each pipeline stage to its
// implementation - inspecting -> internal/audio.Inspect, splitting ->
// internal/audio.Split, asr -> the per-chapter internal/asr loop, sanitizing ->
// internal/transcript normalization - and falls through to a stub for every stage
// a later milestone still owns (the agent stages, contribute, retranscribing). The
// scheduler API is unchanged: it sees one Executor.
//
// Each real stage writes its _done/<stage>.json sentinel as its final durable
// action (the crash-resume contract) and returns the branch decision the state
// machine consults. The composite lives here, not in the scheduler, so the
// scheduler stays free of audio/ASR/tool concerns; server.go constructs it with
// the resolved ffmpeg/ffprobe paths and the selected ASR backend.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/kodestar/audiosilo-sidecars/internal/asr"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/scratch"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// ASRSetup carries the ASR backend the composite executor runs the asr stage on,
// its capability (so a book fails with a clear message when ASR is unavailable),
// and the resolved model/language recorded as transcript provenance.
type ASRSetup struct {
	Backend  asr.Backend
	Cap      asr.Capability
	Model    string
	Language string
}

// Executor is the composite stage executor. ffmpeg/ffprobe are the resolved tool
// paths ("" when unavailable, in which case the audio stages fail their book with
// a clear error while the rest of the daemon keeps working). dataDir is the daemon
// data dir the ASR backend derives its venv/model cache from. fallback runs every
// stage the real executors don't yet implement. db is used to account a book's
// scratch size once a stage has written durable artifacts.
type Executor struct {
	db       *store.DB
	ffmpeg   string
	ffprobe  string
	dataDir  string
	asr      ASRSetup
	fallback scheduler.Executor
}

// NewExecutor builds a composite executor over the resolved tool paths and ASR
// backend, delegating unimplemented stages to fallback (the stub executor). db may
// be nil in tests that don't assert scratch accounting.
func NewExecutor(db *store.DB, ffmpeg, ffprobe, dataDir string, asrSetup ASRSetup, fallback scheduler.Executor) *Executor {
	return &Executor{db: db, ffmpeg: ffmpeg, ffprobe: ffprobe, dataDir: dataDir, asr: asrSetup, fallback: fallback}
}

// Execute routes a stage to its implementation. Inspecting, splitting, asr, and
// sanitizing are real; everything else falls through to the stub.
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
	case state.ASR:
		return e.asrStage(ctx, book, report)
	case state.Sanitizing:
		return e.sanitize(ctx, book, report)
	default:
		return e.fallback.Execute(ctx, book, stage, report)
	}
}

// MarkersNormalizingMsg is the needs_attention reason a book is parked with when
// its chapter markers cannot be normalized automatically yet (M5). Exported so a
// test (and later the UI) can assert/label it exactly.
const MarkersNormalizingMsg = "chapter markers need manual mapping - automatic normalization arrives in a later milestone (M5)"

// asrInfoName is the provenance sidecar the asr stage writes (backend/model/
// language) and the sanitize stage reads to stamp normalized transcripts.
const asrInfoName = "asr.json"

// asrProvenance is the persisted backend/model/language the asr stage used.
type asrProvenance struct {
	Backend  string `json:"backend"`
	Model    string `json:"model"`
	Language string `json:"language"`
}

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
	e.accountScratch(ctx, book)
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

// asrStage transcribes each chapter FLAC to a raw transcript under transcripts-raw/.
// It is RESUMABLE: a chapter whose raw output already parses complete is skipped, a
// malformed/partial one is deleted and re-transcribed, and ctx cancellation returns
// cleanly with completed chapters intact. Each completed raw file is frozen 0444
// (durable evidence, never rewritten by a later stage). The backend runs one
// chapter at a time here, and the scheduler's capacity-1 ASR lane guarantees only
// one book transcribes at a time (Metal-contention constraint). Normalization is a
// separate stage (sanitizing) so the raw layer stays untouched.
func (e *Executor) asrStage(ctx context.Context, book store.Book, report scheduler.ProgressFunc) (scheduler.StageResult, error) {
	if e.asr.Backend == nil || !e.asr.Cap.Available {
		return scheduler.StageResult{}, fmt.Errorf("asr: no speech-recognition backend available: %s", asrUnavailableDetail(e.asr.Cap))
	}
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("asr: read manifest (inspect must run first): %w", err)
	}
	if len(manifest.Chapters) == 0 {
		return scheduler.StageResult{}, fmt.Errorf("asr: manifest has no chapters")
	}
	rawDir := filepath.Join(book.WorkDir, transcript.RawDir)
	if err := os.MkdirAll(rawDir, 0o750); err != nil {
		return scheduler.StageResult{}, err
	}
	// Prepare the backend once per book run (build the venv / fetch the model).
	// Idempotent + logged inside the backend.
	if err := e.asr.Backend.EnsureReady(ctx, e.dataDir); err != nil {
		if ctx.Err() != nil {
			return scheduler.StageResult{}, ctx.Err()
		}
		return scheduler.StageResult{}, fmt.Errorf("asr: prepare %s: %w", e.asr.Backend.ID(), err)
	}

	total := len(manifest.Chapters)
	if report != nil {
		report(0, total)
	}
	for i, ch := range manifest.Chapters {
		if err := ctx.Err(); err != nil {
			return scheduler.StageResult{}, err // clean pause/cancel/shutdown; completed chapters remain
		}
		rawPath := filepath.Join(rawDir, transcript.RawName(ch.Chapter))
		if rawComplete(rawPath) {
			if report != nil {
				report(i+1, total)
			}
			continue // resume: this chapter is already done
		}
		_ = os.Remove(rawPath) // clear any malformed/partial output before retrying
		flac := filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(ch.Chapter))
		if !fileExists(flac) {
			return scheduler.StageResult{}, fmt.Errorf("asr: chapter %d FLAC missing (%s); split must run first", ch.Chapter, flac)
		}
		// InitialPrompt is intentionally empty in M3a: verified spellings come from the
		// spelling stage (M4). Seeding a guess would make a wrong spelling recur.
		job := asr.Job{Audio: flac, OutDir: rawDir, Chapter: ch.Chapter, Language: e.asr.Language}
		if err := e.asr.Backend.Transcribe(ctx, job); err != nil {
			if ctx.Err() != nil {
				return scheduler.StageResult{}, ctx.Err() // killed by cancellation, not a real failure
			}
			return scheduler.StageResult{}, fmt.Errorf("asr: transcribe chapter %d: %w", ch.Chapter, err)
		}
		if !rawComplete(rawPath) {
			return scheduler.StageResult{}, fmt.Errorf("asr: chapter %d produced an incomplete transcript", ch.Chapter)
		}
		// Freeze the raw output: it is durable audit evidence the sanitize stage only
		// reads, so make it read-only (immutability guard). It is a non-secret
		// transcript, so a world-readable read-only mode is intended.
		_ = os.Chmod(rawPath, 0o444) //nolint:gosec // immutability guard on a non-secret artifact
		if report != nil {
			report(i+1, total)
		}
	}

	if err := writeASRProvenance(book.WorkDir, asrProvenance{
		Backend: e.asr.Backend.ID(), Model: e.asr.Model, Language: e.asr.Language,
	}); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("asr: write provenance: %w", err)
	}
	e.accountScratch(ctx, book)

	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{
			"backend":       e.asr.Backend.ID(),
			"model":         e.asr.Model,
			"chapter_count": total,
		}),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.ASR), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// sanitize derives transcripts-json/ (normalized audiosilo-transcript/v1, NaN->null)
// and transcripts-text/ (concatenated segment text) from the immutable
// transcripts-raw/ layer. It is idempotent (each derived file is rewritten) and
// respects ctx cancellation. It never writes into transcripts-raw/.
func (e *Executor) sanitize(ctx context.Context, book store.Book, report scheduler.ProgressFunc) (scheduler.StageResult, error) {
	rawDir := filepath.Join(book.WorkDir, transcript.RawDir)
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("sanitize: read raw transcripts (asr must run first): %w", err)
	}
	var chapters []int
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if n, ok := transcript.ParseChapter(ent.Name()); ok {
			chapters = append(chapters, n)
		}
	}
	if len(chapters) == 0 {
		return scheduler.StageResult{}, fmt.Errorf("sanitize: no raw transcripts found in %s", rawDir)
	}
	sort.Ints(chapters)

	prov := readASRProvenance(book.WorkDir)
	jsonDir := filepath.Join(book.WorkDir, transcript.JSONDir)
	textDir := filepath.Join(book.WorkDir, transcript.TextDir)
	total := len(chapters)
	if report != nil {
		report(0, total)
	}
	for i, chNum := range chapters {
		if err := ctx.Err(); err != nil {
			return scheduler.StageResult{}, err
		}
		raw, err := os.ReadFile(filepath.Join(rawDir, transcript.RawName(chNum))) //nolint:gosec // path derives from the book's work dir
		if err != nil {
			return scheduler.StageResult{}, fmt.Errorf("sanitize: read chapter %d: %w", chNum, err)
		}
		tr, err := transcript.Normalize(raw, transcript.Meta{
			Chapter: chNum, Backend: prov.Backend, Model: prov.Model, Language: prov.Language,
		})
		if err != nil {
			return scheduler.StageResult{}, fmt.Errorf("sanitize: normalize chapter %d: %w", chNum, err)
		}
		if err := transcript.WriteNormalized(jsonDir, tr); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("sanitize: write normalized chapter %d: %w", chNum, err)
		}
		if err := transcript.WriteText(textDir, chNum, transcript.PlainText(tr)); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("sanitize: write text chapter %d: %w", chNum, err)
		}
		if report != nil {
			report(i+1, total)
		}
	}
	e.accountScratch(ctx, book)

	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{"chapter_count": total}),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Sanitizing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// accountScratch re-walks the work dir once and records its on-disk size, so the
// book list and the /system gauge serve scratch_bytes from the DB without walking
// on every read. It uses a non-cancellable context: the stage completed
// successfully, so a shutdown/cancel racing this final gauge write must not drop it
// and leave the column stale (the accounting is the last durable step). A nil db
// (tests) is a no-op.
func (e *Executor) accountScratch(ctx context.Context, book store.Book) {
	if e.db == nil {
		return
	}
	if n, derr := scratch.DirSize(book.WorkDir); derr == nil {
		_ = e.db.UpdateScratchBytes(context.WithoutCancel(ctx), book.ID, n)
	}
}

// rawComplete reports whether a raw transcript file exists and parses as a
// structurally complete transcript (either recognized format), the resume/skip
// test ported from audio_extract.py's transcript_is_complete.
func rawComplete(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return false
	}
	return transcript.Complete(data)
}

// writeASRProvenance records the backend/model/language for the sanitize stage.
func writeASRProvenance(workDir string, p asrProvenance) error {
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(workDir, asrInfoName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil { //nolint:gosec // non-secret artifact
		return err
	}
	return os.Rename(tmp, path)
}

// readASRProvenance loads the asr.json provenance, returning a zero value (blank
// provenance) when it is absent - sanitize still produces valid transcripts, just
// without the backend/model stamp.
func readASRProvenance(workDir string) asrProvenance {
	var p asrProvenance
	raw, err := os.ReadFile(filepath.Join(workDir, asrInfoName)) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return p
	}
	_ = json.Unmarshal(raw, &p)
	return p
}

// asrUnavailableDetail returns a human reason an ASR backend is unavailable.
func asrUnavailableDetail(cap asr.Capability) string {
	if cap.Detail != "" {
		return cap.Detail
	}
	return "no backend configured"
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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
