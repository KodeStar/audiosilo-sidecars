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
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/api"
	"github.com/kodestar/audiosilo-sidecars/internal/auth"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
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
	// Persist every published event to the durable log (fire-and-forget; a log
	// write failure must never break live delivery).
	hub.SetPersister(func(ev events.Event) {
		bookID := bookIDOf(ev.Data)
		_ = db.InsertEvent(context.Background(), time.Now(), ev.Type, bookID, ev.Data)
	})
	go hub.RunHeartbeat(ctx, heartbeatInterval)

	// Pipeline wiring (M1): the metadata client, the async scan manager, and the
	// three-lane scheduler over stub executors. The scheduler runs its own
	// goroutine and reconciles crash state on start.
	metaClient := metaops.NewClient(cfg.Metadata.BaseURL)
	scanMgr := metaops.NewScanManager(ctx, metaClient, cfg.Scan.FFprobePath)
	sched := scheduler.New(db, hub, scheduler.NewStubExecutor(0, 0), cfg.Agent.Concurrency)
	go func() {
		if err := sched.Start(ctx); err != nil {
			fmt.Fprintf(opts.Out, "[error] scheduler stopped: %v\n", err)
		}
	}()

	apiHandler := api.New(api.Deps{
		Auth:      mgr,
		Limiter:   auth.NewRateLimiter(10, 0.5), // burst 10, refill 1 per 2s
		Secrets:   sec,
		Events:    hub,
		Version:   opts.Version,
		DataDir:   opts.DataDir,
		Store:     db,
		Scheduler: sched,
		Scans:     scanMgr,
		Config:    cfg,
		Save:      func(c config.Config) error { return config.Save(opts.DataDir, c) },
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

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return shutdown(srv)
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

// bookIDOf extracts an optional book_id from an event payload so the durable log
// can key book-scoped events (book.state/stage.progress) to their book, while
// daemon-wide events (queue.stats/heartbeat data) store NULL. A payload without
// the field yields 0.
func bookIDOf(data json.RawMessage) int64 {
	if len(data) == 0 {
		return 0
	}
	var probe struct {
		BookID int64 `json:"book_id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return 0
	}
	return probe.BookID
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
