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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/kodestar/audiosilo-sidecars/internal/asr"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/scratch"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/toolfetch"
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

// ToolConfig carries the explicit ffmpeg/ffprobe config paths so the executor can
// re-resolve a tool LOCALLY after startup (an operator who named a binary meant
// that one). Empty fields fall back to the beside-the-binary / $PATH lookup.
type ToolConfig struct {
	FFmpegPath  string
	FFprobePath string
}

// Config configures a composite Executor. FFmpeg/FFprobe are the tool paths
// resolved at startup ("" when unresolved); Tools carries the explicit config
// paths honored on a later re-resolution. ASR is the backend chosen at startup and
// ASRSelect lets a stage re-run asr.Select when that backend was unavailable.
type Config struct {
	DB        *store.DB
	FFmpeg    string
	FFprobe   string
	Tools     ToolConfig
	DataDir   string
	ASR       ASRSetup
	ASRSelect asr.SelectConfig
	Log       *slog.Logger
	Fallback  scheduler.Executor
}

// Executor is the composite stage executor. ffmpeg/ffprobe are the resolved tool
// paths ("" when unavailable, in which case the audio stages PARK their book
// needs_attention with a clear, human-fixable message while the rest of the daemon
// keeps working - a missing tool is a startup precondition a person can fix and
// retry, not a hard failure). Because a tool or ASR backend can appear AFTER
// startup (the operator installs it, then hits Retry), the asr/inspect/split stages
// lazily re-detect on entry under mu; Lane A (cap 1) makes ASR contention trivial,
// but Lane C (cap 2) can re-resolve tools concurrently, so mu guards the mutable
// ffmpeg/ffprobe/asr fields. dataDir is the daemon data dir the ASR backend derives
// its venv/model cache from. fallback runs every stage the real executors don't yet
// implement. db is used to account a book's scratch size once a stage has written
// durable artifacts.
type Executor struct {
	db *store.DB

	mu      sync.Mutex // guards ffmpeg, ffprobe, asr
	ffmpeg  string
	ffprobe string
	asr     ASRSetup

	ffmpegCfg  string
	ffprobeCfg string
	dataDir    string
	asrSelect  asr.SelectConfig
	log        *slog.Logger
	fallback   scheduler.Executor

	// redetectASR re-selects an ASR backend when the frozen one is unavailable. It is
	// a field so a test can inject a scripted result; NewExecutor sets it to
	// defaultRedetectASR (a real asr.Select).
	redetectASR func(context.Context) (asr.Backend, asr.Capability, string)
}

// NewExecutor builds a composite executor from cfg, delegating unimplemented stages
// to cfg.Fallback (the stub executor). cfg.DB may be nil in tests that don't assert
// scratch accounting; cfg.Log nil discards warnings.
func NewExecutor(cfg Config) *Executor {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	e := &Executor{
		db:         cfg.DB,
		ffmpeg:     cfg.FFmpeg,
		ffprobe:    cfg.FFprobe,
		asr:        cfg.ASR,
		ffmpegCfg:  cfg.Tools.FFmpegPath,
		ffprobeCfg: cfg.Tools.FFprobePath,
		dataDir:    cfg.DataDir,
		asrSelect:  cfg.ASRSelect,
		log:        log,
		fallback:   cfg.Fallback,
	}
	e.redetectASR = e.defaultRedetectASR
	return e
}

// ASRCapability returns the executor's current ASR capability (which a stage may
// have re-detected after a retry). Safe for concurrent use.
func (e *Executor) ASRCapability() asr.Capability {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.asr.Cap
}

// ToolPaths returns the executor's current resolved media-tool paths (which a stage
// may have re-detected after a retry). Safe for concurrent use.
func (e *Executor) ToolPaths() (ffmpeg, ffprobe string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ffmpeg, e.ffprobe
}

// ensureASR re-selects a backend when the frozen one is unavailable, adopting a
// now-available result for this and future runs. Detect is cheap (PATH/stat).
func (e *Executor) ensureASR(ctx context.Context) ASRSetup {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.asr.Backend != nil && e.asr.Cap.Available {
		return e.asr
	}
	if b, cap, model := e.redetectASR(ctx); b != nil && cap.Available {
		e.asr.Backend, e.asr.Cap, e.asr.Model = b, cap, model
	}
	return e.asr
}

// defaultRedetectASR re-runs asr.Select and reports a usable backend, or nil when
// none is available. It is the production redetectASR.
func (e *Executor) defaultRedetectASR(ctx context.Context) (asr.Backend, asr.Capability, string) {
	b, cap, err := asr.Select(ctx, e.asrSelect)
	if err != nil || b == nil {
		return nil, asr.Capability{}, ""
	}
	model := e.asrSelect.Model
	if model == "" {
		model = asr.DefaultModelFor(cap.Backend)
	}
	return b, cap, model
}

// ensureTools re-resolves any unresolved media tool LOCALLY (explicit config path
// -> beside the binary -> PATH; no auto-download), adopting a now-present tool.
func (e *Executor) ensureTools() (ffmpeg, ffprobe string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ffmpeg == "" {
		e.ffmpeg = toolfetch.LocateBinary("ffmpeg", e.ffmpegCfg)
	}
	if e.ffprobe == "" {
		e.ffprobe = toolfetch.LocateBinary("ffprobe", e.ffprobeCfg)
	}
	return e.ffmpeg, e.ffprobe
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

// MediaToolsUnavailableMsg is the needs_attention reason the audio stages park a
// book with when ffmpeg/ffprobe could not be resolved at startup. It is a
// known-at-startup, human-fixable precondition (install the tools or enable
// auto-download), so parking - which Retry re-admits - fits better than a hard
// failure. Exported so a test (and the UI) can assert/label it exactly.
const MediaToolsUnavailableMsg = "media tools unavailable: ffmpeg/ffprobe not found - install them or enable auto-download, then retry"

// ManifestChangedMsg parks a book whose manifest fingerprint no longer matches the
// one recorded when transcription began - the existing raw transcripts belong to a
// different edition and must not be silently reused. Exported so tests/UI can assert it.
const ManifestChangedMsg = "source audio or chapter layout changed since transcription - the existing transcripts belong to a different edition; delete this book and re-enqueue to re-transcribe"

// asrInfoName is the provenance sidecar the asr stage writes (backend/model/
// language + the manifest fingerprint and any accepted-empty chapters) and the
// sanitize stage reads to stamp normalized transcripts.
const asrInfoName = "asr.json"

// asrProvenance is the persisted backend/model/language the asr stage used, plus
// the manifest fingerprint that gates a resume against a changed edition and the
// chapters an ASR run accepted as legitimately empty.
type asrProvenance struct {
	Backend       string `json:"backend"`
	Model         string `json:"model"`
	Language      string `json:"language"`
	ManifestSHA   string `json:"manifest_sha,omitempty"`
	EmptyChapters []int  `json:"empty_chapters,omitempty"`
}

// inspect probes the book's source audio, writes probe.json + manifest.json, and
// records whether the chapter markers are contiguous (drives the
// markers_normalizing skip). It writes the stage sentinel as its final action.
func (e *Executor) inspect(ctx context.Context, book store.Book) (scheduler.StageResult, error) {
	_, ffprobe := e.ensureTools()
	if ffprobe == "" {
		return scheduler.StageResult{}, scheduler.Park(MediaToolsUnavailableMsg)
	}
	manifest, contiguous, err := audio.Inspect(ctx, book.SourcePath, book.WorkDir, ffprobe)
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
	ffmpeg, _ := e.ensureTools()
	if ffmpeg == "" {
		return scheduler.StageResult{}, scheduler.Park(MediaToolsUnavailableMsg)
	}
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("split: read manifest (inspect must run first): %w", err)
	}
	if err := audio.Split(ctx, manifest, book.WorkDir, ffmpeg, func(done, total int) {
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
//
// It re-detects the ASR backend on entry (ensureASR) so an operator who installs a
// backend after startup can Retry into it, and it fingerprints the manifest: if the
// recorded fingerprint no longer matches (the source/chapter layout changed since
// transcription began) it parks rather than silently reusing raws for a different
// edition.
func (e *Executor) asrStage(ctx context.Context, book store.Book, report scheduler.ProgressFunc) (scheduler.StageResult, error) {
	setup := e.ensureASR(ctx)
	if setup.Backend == nil || !setup.Cap.Available {
		// A missing ASR backend is a known, human-fixable precondition (install
		// python3+mlx-whisper or a whisper-cli binary), so park the book
		// needs_attention - which Retry re-admits - rather than hard-fail it.
		return scheduler.StageResult{}, scheduler.Park("ASR unavailable: " + asrUnavailableDetail(setup.Cap) + " - fix this, then retry")
	}
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("asr: read manifest (inspect must run first): %w", err)
	}
	if len(manifest.Chapters) == 0 {
		return scheduler.StageResult{}, fmt.Errorf("asr: manifest has no chapters")
	}
	// Guard against reusing raws that belong to a different edition: if the manifest
	// fingerprint changed since transcription began, park (do NOT delete the raws -
	// they are 0444 evidence). An empty prior fingerprint (a work dir from before
	// this guard existed) is treated as unknown, not a mismatch.
	fp, err := manifestFingerprint(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("asr: fingerprint manifest: %w", err)
	}
	prior := readASRProvenance(book.WorkDir)
	if prior.ManifestSHA != "" && prior.ManifestSHA != fp {
		return scheduler.StageResult{}, scheduler.Park(ManifestChangedMsg)
	}
	rawDir := filepath.Join(book.WorkDir, transcript.RawDir)
	if err := os.MkdirAll(rawDir, 0o750); err != nil {
		return scheduler.StageResult{}, err
	}
	// Record the fingerprint at stage START so a crash mid-loop still lets a later
	// resume detect a subsequent manifest change.
	if err := writeASRProvenance(book.WorkDir, asrProvenance{
		Backend: setup.Backend.ID(), Model: setup.Model, Language: setup.Language, ManifestSHA: fp,
	}); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("asr: write provenance: %w", err)
	}
	// Prepare the backend once per book run (fetch the binary/model, build the
	// venv). Idempotent + logged inside the backend. A failure here is an
	// environment/tooling precondition (offline first run, a misconfigured
	// whisper_cli_path), never a book-content error - with auto-download on,
	// Detect is optimistic (no network I/O), so a fresh offline box only trips at
	// this step. Park needs_attention so Retry re-admits the book once the human
	// fixes the environment, rather than hard-failing it.
	if err := setup.Backend.EnsureReady(ctx); err != nil {
		if ctx.Err() != nil {
			return scheduler.StageResult{}, ctx.Err()
		}
		return scheduler.StageResult{}, scheduler.Park(
			"ASR setup failed (" + setup.Backend.ID() + "): " + err.Error() + " - fix this, then retry")
	}

	total := len(manifest.Chapters)
	if report != nil {
		report(0, total)
	}
	var emptyChapters []int
	for i, ch := range manifest.Chapters {
		if err := ctx.Err(); err != nil {
			return scheduler.StageResult{}, err // clean pause/cancel/shutdown; completed chapters remain
		}
		rawPath := filepath.Join(rawDir, transcript.RawName(ch.Chapter))
		if rawComplete(rawPath) {
			// Resume: this chapter is already done. Re-freeze it if a crash landed
			// between the write and the chmod, so the immutability guard always holds.
			if info, serr := os.Stat(rawPath); serr == nil && info.Mode().Perm() != 0o444 {
				if err := os.Chmod(rawPath, 0o444); err != nil { //nolint:gosec // immutability guard on a non-secret artifact
					e.log.Warn("asr: could not re-freeze completed raw", "path", rawPath, "err", err)
				}
			}
			if report != nil {
				report(i+1, total)
			}
			continue
		}
		_ = os.Remove(rawPath) // clear any malformed/partial output before retrying
		flac := filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(ch.Chapter))
		if !fsutil.IsFile(flac) {
			return scheduler.StageResult{}, fmt.Errorf("asr: chapter %d FLAC missing (%s); split must run first", ch.Chapter, flac)
		}
		empty, err := e.transcribeChapter(ctx, setup, flac, rawDir, rawPath, ch.Chapter)
		if err != nil {
			return scheduler.StageResult{}, err
		}
		if empty {
			emptyChapters = append(emptyChapters, ch.Chapter)
		}
		// Freeze the raw output: it is durable audit evidence the sanitize stage only
		// reads, so make it read-only (immutability guard). It is a non-secret
		// transcript, so a world-readable read-only mode is intended. A chmod failure
		// is logged, not fatal - the transcript itself is complete.
		if err := os.Chmod(rawPath, 0o444); err != nil { //nolint:gosec // immutability guard on a non-secret artifact
			e.log.Warn("asr: could not freeze raw transcript", "path", rawPath, "err", err)
		}
		if report != nil {
			report(i+1, total)
		}
	}

	if err := writeASRProvenance(book.WorkDir, asrProvenance{
		Backend: setup.Backend.ID(), Model: setup.Model, Language: setup.Language,
		ManifestSHA: fp, EmptyChapters: emptyChapters,
	}); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("asr: write provenance: %w", err)
	}
	e.accountScratch(ctx, book)

	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{
			"backend":       setup.Backend.ID(),
			"model":         setup.Model,
			"chapter_count": total,
		}),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.ASR), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// transcribeChapter runs the backend for one chapter and guards against an empty
// transcript - a known ASR failure mode (a silent/near-silent decode) that still
// passes the structural Complete check. If the normalized transcript has zero
// segments it deletes the raw and retries ONCE; if it is still empty it accepts it
// (returning empty=true so the caller records the chapter in provenance) rather than
// looping forever. A genuine transcription/completeness error is returned as-is.
func (e *Executor) transcribeChapter(ctx context.Context, setup ASRSetup, flac, rawDir, rawPath string, chapter int) (empty bool, err error) {
	// InitialPrompt is intentionally empty in M3a: verified spellings come from the
	// spelling stage (M4). Seeding a guess would make a wrong spelling recur.
	for attempt := 0; attempt < 2; attempt++ {
		job := asr.Job{Audio: flac, OutDir: rawDir, Chapter: chapter, Language: setup.Language}
		if terr := setup.Backend.Transcribe(ctx, job); terr != nil {
			if ctx.Err() != nil {
				return false, ctx.Err() // killed by cancellation, not a real failure
			}
			return false, fmt.Errorf("asr: transcribe chapter %d: %w", chapter, terr)
		}
		if !rawComplete(rawPath) {
			return false, fmt.Errorf("asr: chapter %d produced an incomplete transcript", chapter)
		}
		isEmpty, cerr := rawIsEmpty(rawPath)
		if cerr != nil {
			return false, fmt.Errorf("asr: chapter %d check empty transcript: %w", chapter, cerr)
		}
		if !isEmpty {
			return false, nil
		}
		if attempt == 0 {
			_ = os.Remove(rawPath) // one retry - a transient empty decode
			continue
		}
	}
	e.log.Warn("asr: chapter produced an empty transcript; accepting", "chapter", chapter)
	return true, nil
}

// sanitize derives transcripts-json/ (normalized audiosilo-transcript/v1, NaN->null)
// and transcripts-text/ (concatenated segment text) from the immutable
// transcripts-raw/ layer. It respects ctx cancellation and never writes into
// transcripts-raw/. It deliberately re-derives EVERY chapter on each run (the
// derivation is cheap and idempotent) rather than tracking per-chapter freshness -
// the raw layer is the single source of truth, so a full re-derive is always
// correct and avoids a staleness-tracking bug class for no measurable cost.
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

// rawIsEmpty reports whether a raw transcript normalizes to zero segments - an empty
// transcription that still passes the structural Complete check. A normalize error
// is NOT treated as empty (it is a different, non-empty failure); the caller only
// uses this to trigger the empty-retry path.
func rawIsEmpty(path string) (bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return false, err
	}
	tr, nerr := transcript.Normalize(raw, transcript.Meta{})
	if nerr != nil {
		return false, nil
	}
	return len(tr.Segments) == 0, nil
}

// manifestFingerprint returns the hex sha256 of the book's manifest.json bytes. The
// manifest is written canonically by audio.WriteManifest, so the digest is stable
// across reads and changes exactly when the inspected source/chapter layout changes.
func manifestFingerprint(workDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, audio.ManifestName)) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// writeASRProvenance records the backend/model/language (plus fingerprint + empty
// chapters) for the sanitize stage and the resume guard.
func writeASRProvenance(workDir string, p asrProvenance) error {
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, asrInfoName), append(out, '\n'), 0o644)
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

// metrics marshals a stage's metrics map, tolerating a marshal failure (metrics
// are advisory - a failure here must not fail the stage).
func metrics(m map[string]any) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}
