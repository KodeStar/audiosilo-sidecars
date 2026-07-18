// Package server wires the daemon together (config, auth, secrets, event hub,
// API, embedded UI) and runs the HTTP server with graceful shutdown. It owns the
// startup banner, which prints the one-time admin password exactly once - on the
// run that first provisions the admin - and never again.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/api"
	"github.com/kodestar/audiosilo-sidecars/internal/asr"
	"github.com/kodestar/audiosilo-sidecars/internal/auth"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/contrib"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/pipeline"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/supervisor"
	"github.com/kodestar/audiosilo-sidecars/internal/toolfetch"
	"github.com/kodestar/audiosilo-sidecars/internal/web"
)

// eventRetention is how long the durable event log is kept; older rows are
// pruned on startup so the log cannot grow without bound.
const eventRetention = 30 * 24 * time.Hour

// heartbeatInterval is how often the event hub emits a keepalive/liveness frame.
const heartbeatInterval = 25 * time.Second

// Options configure a daemon run.
type Options struct {
	DataDir string    // config/auth/secrets directory
	Listen  string    // bind address override; empty uses the config value
	Version string    // build version for /system
	Out     io.Writer // banner destination; defaults to os.Stderr
}

// Run loads configuration, provisions the admin on first run, and serves HTTP
// until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, opts Options) error {
	if opts.Out == nil {
		opts.Out = os.Stderr
	}

	cfg, err := config.Load(opts.DataDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if opts.Listen != "" {
		cfg.Listen = opts.Listen
		if err := cfg.Validate(); err != nil {
			return err
		}
	}
	// Persist config on first run so the file exists for the operator to edit.
	if _, statErr := os.Stat(dataFile(opts.DataDir)); errors.Is(statErr, os.ErrNotExist) {
		if err := config.Save(opts.DataDir, cfg); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
	}

	db, err := store.Open(ctx, filepath.Join(opts.DataDir, store.FileName))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = db.Close() }()
	if n, perr := db.PruneEvents(ctx, time.Now().Add(-eventRetention)); perr != nil {
		return fmt.Errorf("prune events: %w", perr)
	} else if n > 0 {
		fmt.Fprintf(opts.Out, "[info] pruned %d event(s) older than 30 days\n", n)
	}
	// Keep pruning on a daily cadence so a long-running daemon's event log stays
	// bounded (the startup prune alone would let it grow between restarts).
	go pruneEventsLoop(ctx, db, opts.Out)

	mgr := auth.New(db.AuthStore())
	oneTimePassword, err := mgr.EnsureAdmin()
	if err != nil {
		return fmt.Errorf("provision admin: %w", err)
	}

	sec, keychainFallback, err := secrets.Open(opts.DataDir)
	if err != nil {
		return fmt.Errorf("open secrets: %w", err)
	}

	hub := events.NewHub(events.DefaultRingSize)
	// Persist every published event to the durable log via an async, ordered
	// persister: the hub enqueues under its lock (preserving id order) and a single
	// background goroutine does the DB writes, so a slow write never stalls
	// publishers and events are logged in the order the ids were assigned.
	persister := newEventPersister(db, opts.Out, eventQueueSize)
	persister.start()
	hub.SetPersister(persister.enqueue)
	go hub.RunHeartbeat(ctx, heartbeatInterval)

	// Resolve the media tools once at startup (explicit path -> next to the binary
	// -> $PATH -> on-demand download into <data>/tools). The resolved ffprobe feeds
	// both the audio inspect stage and the folder scan (one source of truth); ffmpeg
	// drives the chapter split. A missing tool degrades gracefully: the affected
	// stage fails its book with a clear error while the rest of the daemon works.
	toolLog := slog.New(slog.NewTextHandler(opts.Out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tools := toolfetch.Resolve(ctx, toolfetch.ResolveConfig{
		FFmpegPath:   cfg.Tools.FFmpegPath,
		FFprobePath:  cfg.Tools.FFprobePath,
		AutoDownload: cfg.Tools.AutoDownload,
	}, filepath.Join(opts.DataDir, "tools"), toolLog)
	fmt.Fprintf(opts.Out, "[info] media tools: ffmpeg=%s ffprobe=%s\n",
		toolDisplay(tools.FFmpeg), toolDisplay(tools.FFprobe))

	// Resolve the ASR backend once at startup (auto/mlx-whisper/whisper-cpp). Detect
	// is cheap and side-effect-free; the expensive EnsureReady (venv build / model
	// download) runs lazily on the first book's asr stage. An unavailable backend is
	// surfaced on /system and fails a book's asr stage with a clear message while the
	// rest of the daemon works.
	asrBackend, asrCap, err := asr.Select(ctx, asr.SelectConfig{
		Backend:        cfg.ASR.Backend,
		Model:          cfg.ASR.Model,
		Language:       cfg.ASR.Language,
		WhisperCLIPath: cfg.ASR.WhisperCLIPath,
		AutoDownload:   cfg.Tools.AutoDownload,
		DataDir:        opts.DataDir,
		Log:            toolLog,
	})
	if err != nil {
		return fmt.Errorf("select asr backend: %w", err)
	}
	asrModel := cfg.ASR.Model
	if asrModel == "" {
		asrModel = asr.DefaultModelFor(asrCap.Backend)
	}
	fmt.Fprintf(opts.Out, "[info] asr backend: %s (available=%v device=%s)%s\n",
		asrCap.Backend, asrCap.Available, asrCap.Device, asrDetailSuffix(asrCap))

	// Resolve the agent runner once at startup (auto/claude/codex). Detect is cheap
	// (resolve the binary + a fast --version). agent.Select returns a loud error only
	// for a misconfiguration a person must fix - an unknown backend name, or an
	// explicitly-configured backend/path that does not resolve - so it is fatal (auto
	// mode never errors: it just reports unavailable). An unavailable runner is
	// surfaced on /system and parks a book's agent stages with an actionable message
	// while the rest of the daemon works; a stage re-detects on Retry after an install.
	agentSelect := agent.SelectConfig{
		Backend:    cfg.Agent.Backend,
		ClaudePath: cfg.Agent.ClaudePath,
		CodexPath:  cfg.Agent.CodexPath,
	}
	agentRunner, agentAvail, err := agent.Select(ctx, agentSelect, sec)
	if err != nil {
		return fmt.Errorf("select agent backend: %w", err)
	}
	fmt.Fprintf(opts.Out, "[info] agent backend: %s (available=%v)%s\n",
		agentDisplay(agentAvail.Backend), agentAvail.Available, agentDetailSuffix(agentAvail))

	// Pipeline wiring: the metadata client, the async scan manager (using the
	// resolved ffprobe), and the three-lane scheduler over the composite executor
	// (real inspect/split/asr/sanitize from internal/pipeline, stubs beyond). The
	// scheduler runs its own goroutine (under a child context so it can be stopped
	// independently) and reconciles crash state on start.
	metaClient := metaops.NewClient(cfg.Metadata.BaseURL)
	// The scan manager applies persisted candidate overrides (hide / manual work
	// match) from the store, adapted to metaops' store-agnostic interface.
	overrideSrc := func(ctx context.Context) (map[string]metaops.Override, error) {
		rows, err := db.ListOverrides(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]metaops.Override, len(rows))
		for _, o := range rows {
			out[o.SourcePath] = metaops.Override{Hidden: o.Hidden, WorkID: o.WorkID, WorkTitle: o.WorkTitle}
		}
		return out, nil
	}
	scanMgr := metaops.NewScanManager(ctx, metaClient, tools.FFprobe, overrideSrc)
	workRoot := filepath.Join(opts.DataDir, "work")
	// One GitHub token source shared by the contributing stage (executor) and the
	// core-submit/poller service - both resolve the same PAT-then-`gh auth token`
	// credential, so there is no reason to build two.
	tokenSource := contrib.NewTokenSource(sec)
	agentCapacity := cfg.Agent.Capacity()
	exec := pipeline.NewExecutor(pipeline.Config{
		DB:      db,
		FFmpeg:  tools.FFmpeg,
		FFprobe: tools.FFprobe,
		Tools:   pipeline.ToolConfig{FFmpegPath: cfg.Tools.FFmpegPath, FFprobePath: cfg.Tools.FFprobePath},
		DataDir: opts.DataDir,
		ASR: pipeline.ASRSetup{
			Backend:  asrBackend,
			Cap:      asrCap,
			Model:    asrModel,
			Language: cfg.ASR.Language,
		},
		ASRSelect: asr.SelectConfig{
			Backend:        cfg.ASR.Backend,
			Model:          cfg.ASR.Model,
			Language:       cfg.ASR.Language,
			WhisperCLIPath: cfg.ASR.WhisperCLIPath,
			AutoDownload:   cfg.Tools.AutoDownload,
			DataDir:        opts.DataDir,
			Log:            toolLog,
		},
		Agent:            agentRunner,
		AgentAvail:       agentAvail,
		AgentSelect:      agentSelect,
		AgentModels:      pipeline.AgentModels{Claude: cfg.Agent.Claude, OpenAI: cfg.Agent.OpenAI},
		AgentTimeout:     time.Duration(cfg.Agent.TimeoutMinutes) * time.Minute,
		AgentConcurrency: agentCapacity.GlobalInvocations,
		MaxAgentsPerBook: agentCapacity.MaxAgentsPerBook,
		BookBudgetUSD:    cfg.Agent.BookBudgetUSD,
		Pricing:          cfg.Pricing,
		Secrets:          sec,
		Log:              toolLog,
		Fallback:         scheduler.NewStubExecutor(0, 0),
		// Contribution (M7): the contributing stage resolves the work slug against the
		// metadata client, resolves a GitHub credential (PAT in secrets, else `gh auth
		// token`), and publishes per the configured mode. ContribBaseURL is the GitHub
		// REST base (config.contribution.api_base_url, default api.github.com; overridable
		// for tests and GitHub Enterprise); ExportRoot receives local-mode exports.
		Meta:           metaClient,
		TokenSource:    tokenSource,
		ContribMode:    cfg.Contribution.Mode,
		ContribRepo:    cfg.Contribution.Repo,
		ContribBaseURL: cfg.Contribution.APIBaseURL,
		ExportRoot:     filepath.Join(opts.DataDir, "export"),
	})
	sched := scheduler.New(db, hub, exec, agentCapacity.QueueConcurrency, workRoot, cfg.Contribution.AutoPurge)
	schedCtx, cancelSched := context.WithCancel(ctx)
	defer cancelSched()
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		if err := sched.Start(schedCtx); err != nil {
			fmt.Fprintf(opts.Out, "[error] scheduler stopped: %v\n", err)
		}
	}()

	// Supervision is a separate orchestration ledger/service, not a production stage.
	// The deterministic monitor is safe by default; mutations/model calls remain behind
	// explicit configuration gates.
	var supervisorModel supervisor.Model
	if cfg.Supervisor.ModelAssisted {
		modelRunner := agentRunner
		requestedBackend := cfg.Supervisor.ModelBackend
		if requestedBackend != "" && (modelRunner == nil || modelRunner.ID() != requestedBackend) {
			modelSelect := agentSelect
			modelSelect.Backend = requestedBackend
			if selected, available, selectErr := agent.Select(ctx, modelSelect, sec); selectErr == nil && selected != nil && available.Available {
				modelRunner = selected
			} else {
				modelRunner = nil
			}
		}
		supervisorDir := filepath.Join(opts.DataDir, "supervisor")
		if err := os.MkdirAll(supervisorDir, 0o700); err != nil {
			return fmt.Errorf("create supervisor context dir: %w", err)
		}
		if modelRunner != nil && agent.EnforcesNoTools(modelRunner) {
			supervisorModel = supervisor.NewAgentModel(modelRunner, cfg.Supervisor.Model, supervisorDir,
				time.Duration(cfg.Supervisor.TimeoutSeconds)*time.Second, cfg.Supervisor.MaxTurns, cfg.Pricing)
		}
	}
	supervisorSvc := supervisor.New(db, cfg.Supervisor, cfg.Pricing, supervisorModel, supervisor.Hooks{
		Runtime: func(books []store.Book) supervisor.Runtime {
			r := sched.SupervisorRuntime(books)
			return supervisor.Runtime{ActiveBooks: r.ActiveBooks, AgentActive: r.AgentActive, AgentCapacity: r.AgentCapacity, EligibleAgentBooks: r.EligibleAgentBooks, EligibleAgentIDs: r.EligibleAgentIDs,
				AgentInvocations: r.AgentInvocations, InvocationCapacity: r.InvocationCapacity, InvocationsByBook: r.InvocationsByBook, MaxAgentsPerBook: r.MaxAgentsPerBook}
		},
		Apply: func(ctx context.Context, action supervisor.Action, incident supervisor.Incident) (string, error) {
			if action == supervisor.ActionFallbackBackend {
				if !cfg.Supervisor.AllowBackendFailover {
					return "", errors.New("backend failover is not pre-approved")
				}
				if err := exec.ActivateFallback(ctx, cfg.Supervisor.FallbackBackend, cfg.Supervisor.FallbackModel); err != nil {
					return "", err
				}
				sched.Notify()
				return "pre-approved fallback backend activated within configured concurrency", nil
			}
			return sched.SupervisorApply(ctx, string(action), incident.BookID, incident.Stage)
		},
		Publish: func(eventType string, bookID int64, payload any) {
			if bookID > 0 {
				_ = hub.PublishBook(eventType, bookID, payload)
			} else {
				_ = hub.Publish(eventType, payload)
			}
		},
	})
	supervisorDone := make(chan struct{})
	go func() { defer close(supervisorDone); supervisorSvc.Run(schedCtx) }()

	// Contribution service (M7): the core add-work submit endpoint and the intake
	// poller share it. It reaches the scheduler (re-admit) and the event hub (SSE)
	// through injected function seams so contrib imports neither. VerifyWork maps the
	// metadata client's errors to contrib's local sentinels (a disabled service accepts
	// the slug shape alone). The poller runs under schedCtx so it stops with the
	// scheduler; it works tokenless (public reads).
	contribSvc := contrib.NewService(contrib.ServiceDeps{
		DB:      db,
		Repo:    cfg.Contribution.Repo,
		BaseURL: cfg.Contribution.APIBaseURL, // GitHub REST base (default api.github.com)
		Tokens:  tokenSource,
		Publish: func(u contrib.ContribUpdate) { _ = hub.PublishBook("contrib.update", u.BookID, u) },
		Readmit: sched.Retry,
		VerifyWork: func(ctx context.Context, workID string) error {
			_, err := metaClient.CoverageForWork(ctx, workID)
			switch {
			case err == nil, errors.Is(err, metaops.ErrDisabled):
				return nil
			case errors.Is(err, metaops.ErrWorkNotFound):
				return contrib.ErrWorkNotFound
			default:
				return err
			}
		},
		CorePendingMsg: pipeline.CorePendingMsg,
		Log:            toolLog,
	})
	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		contribSvc.RunPoller(schedCtx, time.Duration(cfg.Contribution.PollMinutes)*time.Minute)
	}()

	apiHandler := api.New(api.Deps{
		Auth:        mgr,
		Limiter:     auth.NewRateLimiter(10, 0.5), // burst 10, refill 1 per 2s
		Secrets:     sec,
		Events:      hub,
		Version:     opts.Version,
		DataDir:     opts.DataDir,
		Store:       db,
		Scheduler:   sched,
		Supervisor:  supervisorSvc,
		Scans:       scanMgr,
		Meta:        metaClient,
		Config:      cfg,
		Save:        func(c config.Config) error { return config.Save(opts.DataDir, c) },
		FFmpegPath:  tools.FFmpeg,
		FFprobePath: tools.FFprobe,
		ASR: api.ASRInfo{
			Backend:   asrCap.Backend,
			Available: asrCap.Available,
			Device:    asrCap.Device,
			Version:   asrCap.Version,
			Detail:    asrCap.Detail,
		},
		// LiveStatus reports the executor's CURRENT ASR capability + resolved tool
		// paths, which a stage may have re-detected after a retry (an operator
		// installing a tool/backend post-startup), so /system reflects them without a
		// daemon restart.
		LiveStatus: func() (api.ASRInfo, string, string) {
			cap := exec.ASRCapability()
			ff, fp := exec.ToolPaths()
			return api.ASRInfo{
				Backend:   cap.Backend,
				Available: cap.Available,
				Device:    cap.Device,
				Version:   cap.Version,
				Detail:    cap.Detail,
			}, ff, fp
		},
		// AgentStatus reports the executor's CURRENT agent-runner availability (which a
		// stage may have re-detected after a retry), so /system reflects an install
		// without a daemon restart.
		AgentStatus: func() api.AgentInfo {
			av := exec.AgentStatus()
			return api.AgentInfo{
				Backend:   av.Backend,
				Available: av.Available,
				Version:   av.Version,
				Detail:    av.Detail,
			}
		},
		// SidecarLoader composes a book's characters/recaps preview from its work dir.
		// The logic lives in pipeline (which api must not import); this adapter marshals
		// it to JSON and translates pipeline's no-sidecars sentinel into the api's so the
		// handler maps it to 404.
		SidecarLoader: func(workDir string) (json.RawMessage, error) {
			raw, err := pipeline.SidecarsViewJSON(workDir)
			if errors.Is(err, pipeline.ErrNoSidecars) {
				return nil, api.ErrNoSidecars
			}
			return raw, err
		},
		// Contribution (M7): the core-submit/set-work service plus the two pipeline
		// loaders (core-proposal prefill + sidecars-zip export), each translating the
		// pipeline's no-data sentinel into the api's so the handlers map them to 404.
		Contrib: contribSvc,
		CoreProposalLoader: func(workDir string) (json.RawMessage, error) {
			raw, err := pipeline.CoreProposalJSON(workDir)
			if errors.Is(err, pipeline.ErrNoCoreProposal) {
				return nil, api.ErrNoCoreProposal
			}
			return raw, err
		},
		ExportArchive: func(b store.Book) ([]byte, string, error) {
			slug := pipeline.ExportSlug(b)
			data, err := pipeline.ExportArchive(b.WorkDir, slug)
			if errors.Is(err, pipeline.ErrNoSidecars) {
				return nil, "", api.ErrNoSidecars
			}
			return data, slug + "-sidecars.zip", err
		},
	}).Handler()

	root := http.NewServeMux()
	root.Handle("/api/v1/", apiHandler)
	root.Handle("/", web.New())

	printBanner(opts.Out, cfg.Listen, opts.DataDir, oneTimePassword, keychainFallback)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE stream is deliberately long-lived.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	var runErr error
	select {
	case err := <-errCh:
		runErr = err
	case <-ctx.Done():
		_ = shutdown(srv)
	}
	// Stop the scheduler and wait for its in-flight workers to fully drain BEFORE
	// the deferred db.Close() runs, so no stage worker writes to a closed database.
	// sched.Start returns only after ctx is cancelled AND wg.Wait completes.
	cancelSched()
	<-schedDone
	<-pollerDone
	<-supervisorDone
	// Drain and stop the durable event persister (its queue may hold late events).
	persister.Close()
	return runErr
}

// eventQueueSize bounds the durable-event persister's backlog. A full queue drops
// the oldest surplus (the live SSE stream already delivered it); the durable log
// is best-effort scaffolding for future log views.
const eventQueueSize = 1024

// eventPersister writes published events to the durable log off the hot path. The
// hub enqueues under its lock (so ids stay ordered) via enqueue, which never
// blocks; a single background goroutine performs the DB writes.
type eventPersister struct {
	db      *store.DB
	out     io.Writer
	ch      chan events.Event
	done    chan struct{}
	dropped atomic.Uint64
}

// newEventPersister builds a persister with a bounded queue. Call start to launch
// the drain goroutine.
func newEventPersister(db *store.DB, out io.Writer, buffer int) *eventPersister {
	if buffer < 1 {
		buffer = 1
	}
	return &eventPersister{
		db:   db,
		out:  out,
		ch:   make(chan events.Event, buffer),
		done: make(chan struct{}),
	}
}

// start launches the single drain goroutine.
func (p *eventPersister) start() { go p.drain() }

// enqueue queues an event for durable persistence. It is called by the hub UNDER
// its lock and MUST never block: on a full queue the event is dropped (best-effort
// log) and a drop counter is bumped and logged.
func (p *eventPersister) enqueue(ev events.Event) {
	select {
	case p.ch <- ev:
	default:
		n := p.dropped.Add(1)
		if n == 1 || n%1000 == 0 {
			fmt.Fprintf(p.out, "[warn] durable event log overloaded; dropped %d event(s) total\n", n)
		}
	}
}

// drain performs the DB writes until the queue is closed, then signals done.
func (p *eventPersister) drain() {
	defer close(p.done)
	for ev := range p.ch {
		_ = p.db.InsertEvent(context.Background(), time.Now(), ev.ID, ev.Type, ev.BookID, ev.Data)
	}
}

// Close stops accepting events and waits for the queue to drain. It must be called
// only after all publishers have stopped (no Publish can race a closed channel).
func (p *eventPersister) Close() {
	close(p.ch)
	<-p.done
}

// pruneDailyInterval is the cadence of the background event-log prune.
const pruneDailyInterval = 24 * time.Hour

// pruneEventsLoop prunes events older than eventRetention once a day until ctx is
// cancelled. A prune failure is logged and retried on the next tick, never fatal.
func pruneEventsLoop(ctx context.Context, db *store.DB, out io.Writer) {
	ticker := time.NewTicker(pruneDailyInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := db.PruneEvents(ctx, time.Now().Add(-eventRetention))
			if err != nil {
				fmt.Fprintf(out, "[warn] daily event prune failed: %v\n", err)
				continue
			}
			if n > 0 {
				fmt.Fprintf(out, "[info] pruned %d event(s) older than 30 days\n", n)
			}
		}
	}
}

// shutdown attempts a graceful stop, then forces open (SSE) connections closed.
func shutdown(srv *http.Server) error {
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		// Long-lived SSE streams will not drain within the grace window; force them.
		_ = srv.Close()
	}
	return nil
}

// dataFile returns the config file path used to detect a first run.
func dataFile(dir string) string {
	return dir + string(os.PathSeparator) + config.FileName
}

// asrDetailSuffix appends the ASR unavailability reason to the startup log line
// when the backend is not ready, so the operator sees why immediately.
func asrDetailSuffix(cap asr.Capability) string {
	if cap.Available || cap.Detail == "" {
		return ""
	}
	return " - " + cap.Detail
}

// agentDisplay renders the resolved agent backend for the startup log, marking an
// absent backend clearly rather than printing an empty string.
func agentDisplay(backend string) string {
	if backend == "" {
		return "(none)"
	}
	return backend
}

// agentDetailSuffix appends the agent unavailability reason to the startup log line
// when the runner is not ready, so the operator sees why immediately.
func agentDetailSuffix(av agent.Availability) string {
	if av.Available || av.Detail == "" {
		return ""
	}
	return " - " + av.Detail
}

// toolDisplay renders a resolved tool path for the startup log, marking an
// unresolved tool clearly rather than printing an empty string.
func toolDisplay(path string) string {
	if path == "" {
		return "(not found)"
	}
	return path
}

// printBanner writes the startup banner. The one-time password is printed only
// when non-empty (first run); it is never persisted or logged elsewhere.
func printBanner(w io.Writer, listen, dataDir, oneTimePassword string, keychainFallback bool) {
	const line = "============================================================"
	fmt.Fprintln(w, line)
	fmt.Fprintln(w, " AudioSilo Sidecars")
	fmt.Fprintf(w, " listening on  http://%s\n", listen)
	fmt.Fprintf(w, " data dir      %s\n", dataDir)
	if oneTimePassword != "" {
		fmt.Fprintln(w, line)
		fmt.Fprintln(w, " FIRST RUN - your one-time admin password (shown ONCE):")
		fmt.Fprintf(w, "\n     %s\n\n", oneTimePassword)
		fmt.Fprintln(w, " Sign in with it, then set your own password in Settings.")
	}
	if keychainFallback {
		fmt.Fprintln(w, line)
		fmt.Fprintf(w, " [warn] no OS keychain available; secrets are stored in\n")
		fmt.Fprintf(w, "        %s%csecrets.json (0600).\n", dataDir, os.PathSeparator)
	}
	fmt.Fprintln(w, line)
}
