// Package api is the transport layer for the audiosilo-sidecars daemon: HTTP
// routing, authentication middleware, CORS, and the JSON/SSE handlers. It holds
// no business logic - auth, secrets, config and the event hub live in their own
// packages and are injected here. Keeping this layer thin keeps the logic
// unit-testable and mirrors the workspace's "api is transport-only" rule.
package api

import (
	"net/http"
	"sync"

	"github.com/kodestar/audiosilo-sidecars/internal/auth"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// Deps are the collaborators the API needs.
type Deps struct {
	Auth    *auth.Manager
	Limiter *auth.RateLimiter
	Secrets secrets.Store
	Events  *events.Hub
	Version string
	DataDir string
	// Store, Scheduler, and Scans back the M1 Library/pipeline endpoints. They may
	// be nil in tests that only exercise the M0 auth/settings surface; the
	// pipeline handlers guard on nil and return 503.
	Store     *store.DB
	Scheduler *scheduler.Scheduler
	Scans     *metaops.ScanManager
	// Config is the loaded configuration. The API owns it after construction and
	// serializes reads/writes; Save persists mutations (e.g. cors_origins) back to
	// config.yaml.
	Config config.Config
	Save   func(config.Config) error
	// FFmpegPath and FFprobePath are the tool paths resolved at startup (empty when
	// a tool could not be located). Surfaced read-only on /system so the UI/operator
	// can see which media tools the audio stages will use.
	FFmpegPath  string
	FFprobePath string
	// ASR is the speech-recognition backend resolved at startup, surfaced read-only
	// on /system so the UI/operator can see whether ASR will run and on what device.
	ASR ASRInfo
	// LiveStatus, when set, supplies the CURRENT ASR capability and resolved media-tool
	// paths (which a stage may have re-detected after a retry), so /system reflects them
	// without a restart. nil falls back to the boot-time ASR/FFmpegPath/FFprobePath.
	LiveStatus func() (ASRInfo, string, string)
}

// ASRInfo is the resolved ASR backend capability shown on /system.
type ASRInfo struct {
	Backend   string `json:"backend"`
	Available bool   `json:"available"`
	Device    string `json:"device"`
	Version   string `json:"version"`
	Detail    string `json:"detail"`
}

// API is the HTTP transport.
type API struct {
	auth       *auth.Manager
	limiter    *auth.RateLimiter
	secrets    secrets.Store
	events     *events.Hub
	version    string
	dataDir    string
	store      *store.DB
	sched      *scheduler.Scheduler
	scans      *metaops.ScanManager
	ffmpeg     string
	ffprobe    string
	asr        ASRInfo
	liveStatus func() (ASRInfo, string, string)

	mu   sync.Mutex // guards cfg
	cfg  config.Config
	save func(config.Config) error
}

// New constructs an API from its dependencies.
func New(d Deps) *API {
	save := d.Save
	if save == nil {
		save = func(config.Config) error { return nil }
	}
	return &API{
		auth:       d.Auth,
		limiter:    d.Limiter,
		secrets:    d.Secrets,
		events:     d.Events,
		version:    d.Version,
		dataDir:    d.DataDir,
		store:      d.Store,
		sched:      d.Scheduler,
		scans:      d.Scans,
		ffmpeg:     d.FFmpegPath,
		ffprobe:    d.FFprobePath,
		asr:        d.ASR,
		liveStatus: d.LiveStatus,
		cfg:        d.Config,
		save:       save,
	}
}

// Handler returns the fully-wired HTTP handler for the /api/v1 surface, with CORS
// and security-header middleware applied. Mount it under "/api/v1/".
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public (rate-limited).
	mux.HandleFunc("POST /api/v1/auth/login", a.handleLogin)

	// Authed.
	mux.HandleFunc("POST /api/v1/auth/logout", a.requireAuth(a.handleLogout))
	mux.HandleFunc("POST /api/v1/auth/password", a.requireAuth(a.handlePassword))
	mux.HandleFunc("GET /api/v1/system", a.requireAuth(a.handleSystem))
	mux.HandleFunc("GET /api/v1/settings", a.requireAuth(a.handleGetSettings))
	mux.HandleFunc("PUT /api/v1/settings", a.requireAuth(a.handlePutSettings))

	// Pipeline / Library (M1). requirePipeline 503s these when the pipeline deps
	// are not wired, composed here so no handler repeats the guard.
	mux.HandleFunc("POST /api/v1/scans", a.requireAuth(a.requirePipeline(a.handleCreateScan)))
	mux.HandleFunc("GET /api/v1/scans/{id}", a.requireAuth(a.requirePipeline(a.handleGetScan)))
	mux.HandleFunc("POST /api/v1/books", a.requireAuth(a.requirePipeline(a.handleCreateBooks)))
	mux.HandleFunc("GET /api/v1/books", a.requireAuth(a.requirePipeline(a.handleListBooks)))
	mux.HandleFunc("GET /api/v1/books/{id}", a.requireAuth(a.requirePipeline(a.handleGetBook)))
	mux.HandleFunc("POST /api/v1/books/{id}/pause", a.requireAuth(a.requirePipeline(a.bookAction((*scheduler.Scheduler).Pause))))
	mux.HandleFunc("POST /api/v1/books/{id}/resume", a.requireAuth(a.requirePipeline(a.bookAction((*scheduler.Scheduler).Resume))))
	mux.HandleFunc("POST /api/v1/books/{id}/retry", a.requireAuth(a.requirePipeline(a.bookAction((*scheduler.Scheduler).Retry))))
	mux.HandleFunc("POST /api/v1/books/{id}/cancel", a.requireAuth(a.requirePipeline(a.bookAction((*scheduler.Scheduler).Cancel))))
	mux.HandleFunc("POST /api/v1/books/{id}/purge-scratch", a.requireAuth(a.requirePipeline(a.handlePurgeScratch)))
	mux.HandleFunc("DELETE /api/v1/books/{id}", a.requireAuth(a.requirePipeline(a.handleDeleteBook)))

	// SSE authenticates itself (token in the query, since EventSource cannot set
	// an Authorization header).
	mux.HandleFunc("GET /api/v1/events", a.handleEvents)

	return a.securityHeaders(a.cors(mux))
}

// origins returns a snapshot of the configured CORS origins.
func (a *API) origins() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.cfg.CORSOrigins))
	copy(out, a.cfg.CORSOrigins)
	return out
}

// snapshot returns a copy of the current config.
func (a *API) snapshot() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}
