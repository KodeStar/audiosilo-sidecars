// Package pipeline wires the real stage executors into the scheduler. It provides a
// composite scheduler.Executor that routes each pipeline stage to its implementation.
// As of M7 EVERY pipeline stage is real (contributing publishes the sidecars to
// KodeStar/audiosilo-meta). The mechanical stages are inspecting -> internal/audio.Inspect,
// splitting -> internal/audio.Split, asr -> the per-chapter internal/asr loop,
// sanitizing -> internal/transcript normalization, qa_sweep -> the internal/qa
// degeneration sweep, retranscribing -> internal/repair (tail-clip + adoption),
// correcting -> the internal/spelling engine, and validating -> canonicalize +
// audiosilo-meta n-gram. The AGENT stages (markers_normalizing, qa_adjudicating,
// spelling_research, fact_pass, synthesizing, auditing, fixing) run internal/agent
// through the shared runAgent driver. The scheduler API is unchanged: it sees one
// Executor.
//
// A stage that cannot proceed on a human-fixable precondition (media tools or an agent
// backend unavailable, a not-confident marker verdict, a spelling gate failure, a
// non-converging QA/fix loop) PARKS the book needs_attention (Retry re-admits it)
// rather than failing; only a genuine error fails a book.
//
// Each real stage writes its _done/<stage>.json sentinel as its final durable action
// (the crash-resume contract) and returns the branch decision the state machine
// consults. The composite lives here, not in the scheduler, so the scheduler stays
// free of audio/ASR/agent/tool concerns; server.go constructs it with the resolved
// ffmpeg/ffprobe paths, the selected ASR backend, and the selected agent runner.
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
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/asr"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/pricing"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/repair"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/scratch"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
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

// AgentModels routes each agent stage to a model per backend. Claude keys the claude
// backend, OpenAI the codex backend; a missing key means the backend CLI's default
// model. It is a plain-map view of config.AgentConfig.Claude/.OpenAI so the pipeline
// package never imports internal/config.
type AgentModels struct {
	Claude map[string]string
	OpenAI map[string]string
}

// Config configures a composite Executor. FFmpeg/FFprobe are the tool paths
// resolved at startup ("" when unresolved); Tools carries the explicit config
// paths honored on a later re-resolution. ASR is the backend chosen at startup and
// ASRSelect lets a stage re-run asr.Select when that backend was unavailable.
//
// The Agent* fields drive the agent stages. Agent is the runner chosen at startup
// (nil when no CLI was available); AgentAvail is its availability (surfaced on
// /system); AgentSelect lets a stage re-run agent.Select when the runner was
// unavailable at startup (the operator installs a CLI, then Retry); AgentModels is
// the per-stage model routing; AgentTimeout bounds one invocation; Secrets is
// threaded so a lazy re-Select can inject an API key into the child env.
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

	Agent        agent.Runner
	AgentAvail   agent.Availability
	AgentSelect  agent.SelectConfig
	AgentModels  AgentModels
	AgentTimeout time.Duration
	// AgentConcurrency is the effective global invocation cap inside the executor.
	// MaxAgentsPerBook independently bounds safe fan-out within one book. Keeping
	// AgentConcurrency's field name here avoids breaking test/embedding callers; the
	// user-facing config calls it the effective global invocation limit.
	AgentConcurrency int
	MaxAgentsPerBook int
	// BookBudgetUSD caps the total agent spend for one book: an agent stage parks the
	// book budget_exceeded (with everything already recorded) once its summed cost reaches
	// this, before spending more. 0 disables the guard (config seeds a large default; set
	// a very large value to effectively disable). From config.agent.book_budget_usd.
	BookBudgetUSD float64
	Pricing       pricing.Table
	Secrets       secrets.Store

	// Contribution (M7) drives the contributing stage. Meta resolves a book's work
	// slug and reads sidecar coverage (nil = metadata disabled); TokenSource resolves a
	// GitHub credential for issue/pr modes (nil = no credential); ContribMode is
	// issue|pr|local; ContribRepo is the upstream owner/name; ContribBaseURL overrides
	// the GitHub REST base for tests (empty = api.github.com); ExportRoot is where local
	// mode writes the repo-layout export.
	Meta           MetaCoverage
	TokenSource    TokenResolver
	ContribMode    string
	ContribRepo    string
	ContribBaseURL string
	ExportRoot     string
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

	mu         sync.Mutex // guards ffmpeg, ffprobe, asr, agentRunner, agentAvail
	ffmpeg     string
	ffprobe    string
	asr        ASRSetup
	agentRun   agent.Runner
	agentAvail agent.Availability

	ffmpegCfg             string
	ffprobeCfg            string
	dataDir               string
	asrSelect             asr.SelectConfig
	agentSelect           agent.SelectConfig
	agentModels           AgentModels
	agentTimeout          time.Duration
	agentWorkers          int // per-book fan-out ceiling
	agentSlots            chan struct{}
	invocationMu          sync.Mutex
	activeInvocations     map[int64]int
	globalInvocationLimit int
	bookBudgetUSD         float64
	pricing               pricing.Table
	secrets               secrets.Store
	log                   *slog.Logger
	fallback              scheduler.Executor

	// Contribution-stage deps (M7); see Config.
	meta           MetaCoverage
	tokenSource    TokenResolver
	contribMode    string
	contribRepo    string
	contribBaseURL string
	exportRoot     string

	// redetectASR re-selects an ASR backend when the frozen one is unavailable. It is
	// a field so a test can inject a scripted result; NewExecutor sets it to
	// defaultRedetectASR (a real asr.Select).
	redetectASR func(context.Context) (asr.Backend, asr.Capability, string)
	// redetectAgent re-selects an agent runner when the frozen one is unavailable,
	// mirroring redetectASR. A test injects a scripted result; NewExecutor sets it to
	// defaultRedetectAgent (a real agent.Select).
	redetectAgent func(context.Context) (agent.Runner, agent.Availability)
	// clipCutter, when non-nil, overrides the ffmpeg-based tail-clip cutter (tests
	// inject a fake so the retranscribing tail-clip path needs no real ffmpeg). nil in
	// production - the retranscribing stage builds repair.FFmpegClipCutter.
	clipCutter repair.ClipCutter
	// backoff, when non-nil, overrides the agent rate-limit backoff schedule runAgent
	// uses; nil uses agent.DefaultBackoff. A test injects a tiny schedule so a
	// rate-limit retry path does not sleep for real minutes.
	backoff []time.Duration
}

// NewExecutor builds a composite executor from cfg, delegating unimplemented stages
// to cfg.Fallback (the stub executor). cfg.DB may be nil in tests that don't assert
// scratch accounting; cfg.Log nil discards warnings.
func NewExecutor(cfg Config) *Executor {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	globalWorkers := max(cfg.AgentConcurrency, 1)
	agentWorkers := cfg.MaxAgentsPerBook
	if agentWorkers < 1 {
		// Compatibility for existing constructors/tests: the old single value was
		// both the global semaphore and fact-pass worker count.
		agentWorkers = globalWorkers
	}
	e := &Executor{
		db:                    cfg.DB,
		ffmpeg:                cfg.FFmpeg,
		ffprobe:               cfg.FFprobe,
		asr:                   cfg.ASR,
		agentRun:              cfg.Agent,
		agentAvail:            cfg.AgentAvail,
		ffmpegCfg:             cfg.Tools.FFmpegPath,
		ffprobeCfg:            cfg.Tools.FFprobePath,
		dataDir:               cfg.DataDir,
		asrSelect:             cfg.ASRSelect,
		agentSelect:           cfg.AgentSelect,
		agentModels:           cfg.AgentModels,
		agentTimeout:          cfg.AgentTimeout,
		agentWorkers:          agentWorkers,
		agentSlots:            make(chan struct{}, globalWorkers),
		activeInvocations:     make(map[int64]int),
		globalInvocationLimit: globalWorkers,
		bookBudgetUSD:         cfg.BookBudgetUSD,
		pricing:               cfg.Pricing,
		secrets:               cfg.Secrets,
		log:                   log,
		fallback:              cfg.Fallback,

		meta:           cfg.Meta,
		tokenSource:    cfg.TokenSource,
		contribMode:    cfg.ContribMode,
		contribRepo:    cfg.ContribRepo,
		contribBaseURL: cfg.ContribBaseURL,
		exportRoot:     cfg.ExportRoot,
	}
	e.redetectASR = e.defaultRedetectASR
	e.redetectAgent = e.defaultRedetectAgent
	return e
}

// AgentInvocationRuntime exposes live executor occupancy without coupling the
// scheduler to pipeline implementation details.
func (e *Executor) AgentInvocationRuntime() (total int, byBook map[int64]int, capacity int) {
	e.invocationMu.Lock()
	defer e.invocationMu.Unlock()
	byBook = make(map[int64]int, len(e.activeInvocations))
	for id, n := range e.activeInvocations {
		byBook[id], total = n, total+n
	}
	return total, byBook, e.globalInvocationLimit
}

func (e *Executor) AgentMaxPerBook() int { return e.agentWorkers }

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

// readyASR is the shared ensure-ASR-or-park preamble the asr and retranscribing
// stages both open with: re-select a backend (adopting one installed after startup),
// park needs_attention when none is available, then prepare it (fetch the binary/
// model, build the venv) - an EnsureReady failure is an environment precondition, so
// it parks (ctx cancellation propagates cleanly). The park strings are the exact ones
// tests assert; keep them byte-identical.
func (e *Executor) readyASR(ctx context.Context) (ASRSetup, error) {
	setup := e.ensureASR(ctx)
	if setup.Backend == nil || !setup.Cap.Available {
		return setup, scheduler.ParkWithCode(state.ParkASRUnavailable, "ASR unavailable: "+asrUnavailableDetail(setup.Cap)+" - fix this, then retry")
	}
	if err := setup.Backend.EnsureReady(ctx); err != nil {
		if ctx.Err() != nil {
			return setup, ctx.Err()
		}
		return setup, scheduler.ParkWithCode(state.ParkASRUnavailable, "ASR setup failed ("+setup.Backend.ID()+"): "+err.Error()+" - fix this, then retry")
	}
	return setup, nil
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

// Execute routes a stage to its implementation. As of M7 every pipeline stage is
// handled here (contributing landed in M7); the fallback runs only an unrecognized
// state, which the state machine never produces.
func (e *Executor) Execute(ctx context.Context, book store.Book, stage state.State, r scheduler.StageReport) (scheduler.StageResult, error) {
	switch stage {
	case state.Inspecting:
		return e.inspect(ctx, book)
	case state.MarkersNormalizing:
		return e.markersNormalize(ctx, book, r)
	case state.Splitting:
		return e.split(ctx, book, r)
	case state.ASR:
		return e.asrStage(ctx, book, r)
	case state.Sanitizing:
		return e.sanitize(ctx, book, r)
	case state.QASweep:
		return e.qaSweep(ctx, book, r)
	case state.QAAdjudicating:
		return e.qaAdjudicate(ctx, book, r)
	case state.Retranscribing:
		return e.retranscribe(ctx, book, r)
	case state.SpellingResearch:
		return e.spellingResearch(ctx, book, r)
	case state.Correcting:
		return e.correcting(ctx, book, r)
	case state.FactPass:
		return e.factPass(ctx, book, r)
	case state.Synthesizing:
		return e.synthesize(ctx, book, r)
	case state.Validating:
		return e.validateSidecarsStage(ctx, book, r)
	case state.Auditing:
		return e.audit(ctx, book, r)
	case state.Fixing:
		return e.fixSidecars(ctx, book, r)
	case state.Contributing:
		return e.contribute(ctx, book, r)
	default:
		return e.fallback.Execute(ctx, book, stage, r)
	}
}

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
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkMediaToolsUnavailable, MediaToolsUnavailableMsg)
	}
	// Time only the productive body (after the tool-availability check) for the rate.
	start := time.Now()
	manifest, contiguous, err := audio.Inspect(ctx, book.SourcePath, book.WorkDir, ffprobe)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("inspect: %w", err)
	}
	// Record the manifest chapter count so the ETA engine has a real per-book total
	// for the per-chapter stages instead of its default. Best-effort bookkeeping (nil
	// db in tests, or a lost row); a failure never fails the stage.
	if e.db != nil {
		_ = e.db.SetBookChapters(context.WithoutCancel(ctx), book.ID, manifest.ChapterCount)
		_ = e.db.SetBookDuration(context.WithoutCancel(ctx), book.ID, manifest.Duration)
	}
	result := scheduler.StageResult{
		MarkersContiguous: contiguous,
		Metrics: metrics(map[string]any{
			"style":         manifest.Style,
			"chapter_count": manifest.ChapterCount,
			"duration_sec":  manifest.Duration,
			"contiguous":    contiguous,
		}),
		RateSample: rateSample(1, time.Since(start).Seconds()),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Inspecting), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// split converts each manifest chapter into a mono/16 kHz FLAC, reporting progress
// per chapter, then writes the stage sentinel.
func (e *Executor) split(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	ffmpeg, _ := e.ensureTools()
	if ffmpeg == "" {
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkMediaToolsUnavailable, MediaToolsUnavailableMsg)
	}
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("split: read manifest (inspect must run first): %w", err)
	}
	// Fail closed: inspect now writes a draft manifest even for non-contiguous markers
	// (so markers_normalizing has something to correct), so a routing regression could
	// otherwise let split cut FLACs straight through a book at its credit/sample
	// boundaries. A non-contiguous manifest here means markers_normalizing did not run
	// (or did not replace the draft) - a loud error, never a silent bad split.
	if !audio.Contiguous(manifest.Chapters) {
		return scheduler.StageResult{}, fmt.Errorf("split: manifest is not contiguous - markers_normalizing must run first")
	}
	// Capture the resume baseline (first report) and final done so the rate counts only
	// chapters split THIS run, not any a prior interrupted run already produced. Time the
	// split loop itself (setup and the manifest read are excluded).
	firstDone, lastDone := 0, 0
	haveFirst := false
	splitStart := time.Now()
	if err := audio.Split(ctx, manifest, book.WorkDir, ffmpeg, func(done, total int) {
		if !haveFirst {
			firstDone, haveFirst = done, true
		}
		lastDone = done
		if r.Progress != nil {
			r.Progress(done, total)
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
		RateSample: rateSample(lastDone-firstDone, time.Since(splitStart).Seconds()),
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
func (e *Executor) asrStage(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	// Ensure the backend is selected AND prepared (or park needs_attention). A missing
	// backend is a known, human-fixable precondition (install python3+mlx-whisper or a
	// whisper-cli binary); the fetch/venv build behind EnsureReady is likewise an
	// environment precondition, never a book-content error - Retry re-admits the book
	// once the human fixes it.
	setup, err := e.readyASR(ctx)
	if err != nil {
		return scheduler.StageResult{}, err
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
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkManifestChanged, ManifestChangedMsg)
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
	total := len(manifest.Chapters)
	// Progress baseline: count the chapters already transcribed on entry (a resume) so
	// the FIRST report reflects prior work and an already-complete chapter never ticks
	// the counter. This keeps the scheduler's EWMA unit span (first..last reported done)
	// measuring only what THIS run actually transcribed - a resume that did 3 of 84
	// records 3 units, not 84. The predicate matches the loop's skip test (rawComplete).
	completed := 0
	for _, ch := range manifest.Chapters {
		if rawComplete(filepath.Join(rawDir, transcript.RawName(ch.Chapter))) {
			completed++
		}
	}
	done := completed
	if r.Progress != nil {
		r.Progress(done, total)
	}
	// Time only the transcription loop (backend selection, model/binary download via
	// readyASR, and the provenance write are excluded), and count only chapters this run
	// actually transcribes (done - completed), so the learned rate is not skewed by a
	// resume or a first-run model fetch.
	asrStart := time.Now()
	var emptyChapters []int
	for _, ch := range manifest.Chapters {
		if err := ctx.Err(); err != nil {
			return scheduler.StageResult{}, err // clean pause/cancel/shutdown; completed chapters remain
		}
		rawPath := filepath.Join(rawDir, transcript.RawName(ch.Chapter))
		if rawComplete(rawPath) {
			// Resume: this chapter is already done (counted in the baseline above). Re-freeze
			// it if a crash landed between the write and the chmod, so the immutability guard
			// always holds, but do NOT re-tick progress - it was not processed this run.
			if info, serr := os.Stat(rawPath); serr == nil && info.Mode().Perm() != 0o444 {
				if err := os.Chmod(rawPath, 0o444); err != nil { //nolint:gosec // immutability guard on a non-secret artifact
					e.log.Warn("asr: could not re-freeze completed raw", "path", rawPath, "err", err)
				}
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
		done++
		if r.Progress != nil {
			r.Progress(done, total)
		}
	}
	asrSeconds := time.Since(asrStart).Seconds()

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
		RateSample: rateSample(done-completed, asrSeconds),
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
	for attempt := range 2 {
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

// deriveChapterLayers derives a chapter's normalized JSON + plain-text layers from its
// raw ASR bytes: Normalize -> WriteNormalized -> WriteText. It is the single owner of
// that sequence, shared by the sanitize loop and the retranscribe adopt path so the two
// cannot drift. meta.Chapter selects the output file names; the raw layer is never
// touched (it is the immutable source).
func deriveChapterLayers(workDir string, raw []byte, meta transcript.Meta) error {
	tr, err := transcript.Normalize(raw, meta)
	if err != nil {
		return fmt.Errorf("normalize chapter %d: %w", meta.Chapter, err)
	}
	if err := transcript.WriteNormalized(filepath.Join(workDir, transcript.JSONDir), tr); err != nil {
		return fmt.Errorf("write normalized chapter %d: %w", meta.Chapter, err)
	}
	if err := transcript.WriteText(filepath.Join(workDir, transcript.TextDir), meta.Chapter, transcript.PlainText(tr)); err != nil {
		return fmt.Errorf("write text chapter %d: %w", meta.Chapter, err)
	}
	return nil
}

// sanitize derives transcripts-json/ (normalized audiosilo-transcript/v1, NaN->null)
// and transcripts-text/ (concatenated segment text) from the immutable
// transcripts-raw/ layer. It respects ctx cancellation and never writes into
// transcripts-raw/. It deliberately re-derives EVERY chapter on each run (the
// derivation is cheap and idempotent) rather than tracking per-chapter freshness -
// the raw layer is the single source of truth, so a full re-derive is always
// correct and avoids a staleness-tracking bug class for no measurable cost.
func (e *Executor) sanitize(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
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
	total := len(chapters)
	if r.Progress != nil {
		r.Progress(0, total)
	}
	// sanitize re-derives EVERY chapter each run, so its units are the whole chapter
	// count; time just the derive loop.
	deriveStart := time.Now()
	for i, chNum := range chapters {
		if err := ctx.Err(); err != nil {
			return scheduler.StageResult{}, err
		}
		raw, err := os.ReadFile(filepath.Join(rawDir, transcript.RawName(chNum))) //nolint:gosec // path derives from the book's work dir
		if err != nil {
			return scheduler.StageResult{}, fmt.Errorf("sanitize: read chapter %d: %w", chNum, err)
		}
		meta := transcript.Meta{Chapter: chNum, Backend: prov.Backend, Model: prov.Model, Language: prov.Language}
		if err := deriveChapterLayers(book.WorkDir, raw, meta); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("sanitize: %w", err)
		}
		if r.Progress != nil {
			r.Progress(i+1, total)
		}
	}
	deriveSeconds := time.Since(deriveStart).Seconds()
	e.accountScratch(ctx, book)

	result := scheduler.StageResult{
		Metrics:    metrics(map[string]any{"chapter_count": total}),
		RateSample: rateSample(total, deriveSeconds),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Sanitizing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// qaSweep runs the mechanical degeneration sweep (internal/qa) over the normalized
// transcripts the sanitize stage wrote, writes qa_report.json + qa_report.md into the
// work dir, and reports QAClean so the state machine branches to spelling_research
// (clean) or qa_adjudicating (dirty). It reads chapter durations from the manifest
// (a wph outlier is words-per-hour, so the sweep needs each chapter's length); a
// missing manifest or missing transcripts point at an out-of-order run, so they are
// loud errors naming the stage that must precede this one. The detectors are fast and
// fully in-memory, so a single ctx check at entry is enough - there is no long inner
// loop to cancel. No scratch accounting: the two reports are tiny.
func (e *Executor) qaSweep(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if err := ctx.Err(); err != nil {
		return scheduler.StageResult{}, err
	}
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("qa_sweep: read manifest (inspect must run first): %w", err)
	}
	durations := make(map[int]float64, len(manifest.Chapters))
	for _, ch := range manifest.Chapters {
		durations[ch.Chapter] = ch.Duration
	}
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	start := time.Now()
	rep, err := qa.Run(qa.Input{WorkDir: book.WorkDir, Durations: durations})
	if err != nil {
		// The sweep reads transcripts-json/, which the sanitizing stage produces; a read
		// failure here means sanitizing has not run (or produced no output) yet.
		return scheduler.StageResult{}, fmt.Errorf("qa_sweep: degeneration sweep (sanitizing must run first): %w", err)
	}
	if err := qa.WriteReport(book.WorkDir, rep); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("qa_sweep: write report: %w", err)
	}
	sweepSeconds := time.Since(start).Seconds()
	if r.Progress != nil {
		r.Progress(1, 1)
	}

	midChapterRuns := 0
	for _, run := range rep.RepeatedRuns {
		if run.Kind == qa.KindMidChapter {
			midChapterRuns++
		}
	}
	result := scheduler.StageResult{
		QAClean: rep.Clean(),
		Metrics: metrics(map[string]any{
			// chapter_count keeps the cross-stage meaning (the book's manifest
			// chapter count, like inspect/split/asr/sanitize) - NOT qa's internal
			// wph-stats count, which excludes chapter 0 and lives in qa_report.json.
			"chapter_count":      len(manifest.Chapters),
			"wph_outliers":       len(rep.WPHOutliers),
			"mid_chapter_runs":   midChapterRuns,
			"cross_segment":      len(rep.CrossSegment),
			"within_segment":     len(rep.WithinSegment),
			"multi_loop":         len(rep.MultiLoop),
			"tail_rate":          len(rep.TailRate),
			"retranscribe_queue": len(rep.RetranscribeQueue),
		}),
		RateSample: rateSample(1, sweepSeconds),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.QASweep), result); err != nil {
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

// hexSHA256 returns the hex-encoded sha256 over the concatenation of parts (one digest
// over all of them, in order). A nil part contributes nothing, so a missing optional
// input hashes as empty. It is the fingerprint primitive for manifestFingerprint.
func hexSHA256(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// manifestFingerprint returns the hex sha256 of the book's manifest.json bytes. The
// manifest is written canonically by audio.WriteManifest, so the digest is stable
// across reads and changes exactly when the inspected source/chapter layout changes.
func manifestFingerprint(workDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, audio.ManifestName)) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return "", err
	}
	return hexSHA256(raw), nil
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

// rateSample builds the StageResult.RateSample a stage reports for the scheduler's EWMA
// rate: units of work processed THIS run and the productive seconds spent on them. It
// returns nil (no observation) for a non-positive units/seconds, so a resumed run that
// did nothing new, or a stage that measured no productive time, updates no rate.
func rateSample(units int, seconds float64) *scheduler.RateSample {
	if units <= 0 || seconds <= 0 {
		return nil
	}
	return &scheduler.RateSample{Units: units, Seconds: seconds}
}
