// Package api is the transport layer for the audiosilo-sidecars daemon: HTTP
// routing, authentication middleware, CORS, and the JSON/SSE handlers. It holds
// no business logic - auth, secrets, config and the event hub live in their own
// packages and are injected here. Keeping this layer thin keeps the logic
// unit-testable and mirrors the workspace's "api is transport-only" rule.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/kodestar/audiosilo-sidecars/internal/auth"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/contrib"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/supervisor"
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
	Store      *store.DB
	Scheduler  *scheduler.Scheduler
	Supervisor *supervisor.Service
	Scans      *metaops.ScanManager
	// Meta is the community-metadata client backing the manual-match and
	// meta-search endpoints. It may be nil (metadata unconfigured); the handlers
	// that need it guard on nil / the disabled state and return 503.
	Meta *metaops.Client
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
	// AgentStatus, when set, supplies the CURRENT agent-runner availability (backend
	// resolved at startup, re-detectable after a CLI install), surfaced read-only on
	// /system. nil -> a zero AgentInfo (no backend, unavailable), mirroring the ASR
	// LiveStatus pattern.
	AgentStatus func() AgentInfo
	// SidecarLoader composes a book's characters/recaps preview from its work dir,
	// returning the metaserve-API-shaped JSON the Done panel renders. It must return
	// ErrNoSidecars when neither sidecar file exists (mapped to 404). The loader lives
	// in internal/pipeline and is injected here so the api never imports pipeline
	// (dependency direction). nil -> the /sidecars endpoint 503s.
	SidecarLoader func(workDir string) (json.RawMessage, error)
	// Contrib is the M7 contribution service (core add-work submit + manual set-work).
	// The API is transport-only over it: it calls Contrib and maps typed errors to
	// status codes. nil disables the contribute/core + work endpoints (503).
	Contrib *contrib.Service
	// CoreProposalLoader returns a book's prefilled contrib/core_proposal.json bytes,
	// or ErrNoCoreProposal when absent (mapped to 404). Injected from pipeline (api
	// must not import it); nil -> the contrib/core GET 503s.
	CoreProposalLoader func(workDir string) (json.RawMessage, error)
	// ExportArchive builds a book's sidecars zip plus its download filename, returning
	// ErrNoSidecars when the book has no sidecars (mapped to 404). Injected from
	// pipeline; nil -> the export endpoint 503s.
	ExportArchive func(b store.Book) (data []byte, filename string, err error)
	// Restart requests a graceful in-process daemon restart. The server injects a
	// non-blocking signal; nil leaves the endpoint unavailable in narrow tests.
	Restart func()
}

// ASRInfo is the resolved ASR backend capability shown on /system.
type ASRInfo struct {
	Backend   string `json:"backend"`
	Available bool   `json:"available"`
	Device    string `json:"device"`
	Version   string `json:"version"`
	Detail    string `json:"detail"`
}

// AgentInfo is the resolved agent-runner capability shown on /system: which backend
// (claude/codex/"") will run the agent stages and whether it is usable.
type AgentInfo struct {
	Backend   string `json:"backend"`
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// API is the HTTP transport.
type API struct {
	auth          *auth.Manager
	limiter       *auth.RateLimiter
	secrets       secrets.Store
	events        *events.Hub
	version       string
	dataDir       string
	store         *store.DB
	sched         *scheduler.Scheduler
	supervisor    *supervisor.Service
	scans         *metaops.ScanManager
	meta          *metaops.Client
	overrides     *metaops.OverrideService
	ffmpeg        string
	ffprobe       string
	asr           ASRInfo
	liveStatus    func() (ASRInfo, string, string)
	agentStatus   func() AgentInfo
	sidecarLoader func(workDir string) (json.RawMessage, error)

	contrib            *contrib.Service
	coreProposalLoader func(workDir string) (json.RawMessage, error)
	exportArchive      func(b store.Book) ([]byte, string, error)
	restart            func()

	mu   sync.Mutex // guards cfg
	cfg  config.Config
	save func(config.Config) error
}

// ErrNoSidecars signals that a book's work dir holds no sidecar files yet, so the
// /sidecars (and /export) endpoints answer 404. The pipeline loader returns its own
// sentinel; the server-side adapter translates it to this one so the api never
// imports pipeline.
var ErrNoSidecars = errors.New("no sidecars")

// ErrNoCoreProposal signals that a book's work dir holds no prefilled core proposal,
// so the contrib/core GET answers 404. The pipeline loader returns its own sentinel;
// the server-side adapter translates it to this one.
var ErrNoCoreProposal = errors.New("no core proposal")

// New constructs an API from its dependencies.
func New(d Deps) *API {
	save := d.Save
	if save == nil {
		save = func(config.Config) error { return nil }
	}
	a := &API{
		auth:          d.Auth,
		limiter:       d.Limiter,
		secrets:       d.Secrets,
		events:        d.Events,
		version:       d.Version,
		dataDir:       d.DataDir,
		store:         d.Store,
		sched:         d.Scheduler,
		supervisor:    d.Supervisor,
		scans:         d.Scans,
		meta:          d.Meta,
		ffmpeg:        d.FFmpegPath,
		ffprobe:       d.FFprobePath,
		asr:           d.ASR,
		liveStatus:    d.LiveStatus,
		agentStatus:   d.AgentStatus,
		sidecarLoader: d.SidecarLoader,

		contrib:            d.Contrib,
		coreProposalLoader: d.CoreProposalLoader,
		exportArchive:      d.ExportArchive,
		restart:            d.Restart,

		cfg:  d.Config,
		save: save,
	}
	// The override-upsert workflow lives in metaops (transport-only handler over
	// it). It needs the store + scan manager; requirePipeline gates the handler
	// on the same deps, so the service is present whenever the handler runs. The
	// persist func adapts metaops' store-agnostic row to the store, so metaops
	// never imports store.
	if d.Store != nil && d.Scans != nil {
		a.overrides = metaops.NewOverrideService(d.Meta, d.Scans,
			func(ctx context.Context, ov metaops.StoredOverride) (metaops.StoredOverride, error) {
				saved, err := d.Store.UpsertOverride(ctx, store.Override{
					SourcePath: ov.SourcePath, Hidden: ov.Hidden,
					WorkID: ov.WorkID, WorkTitle: ov.WorkTitle,
				})
				if err != nil {
					return metaops.StoredOverride{}, err
				}
				return metaops.StoredOverride{
					SourcePath: saved.SourcePath, Hidden: saved.Hidden,
					WorkID: saved.WorkID, WorkTitle: saved.WorkTitle, UpdatedAt: saved.UpdatedAt,
				}, nil
			})
	}
	return a
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
	mux.HandleFunc("POST /api/v1/system/restart", a.requireAuth(a.handleRestart))
	mux.HandleFunc("GET /api/v1/settings", a.requireAuth(a.handleGetSettings))
	mux.HandleFunc("PUT /api/v1/settings", a.requireAuth(a.handlePutSettings))

	// Pipeline / Library (M1). requirePipeline 503s these when the pipeline deps
	// are not wired, composed here so no handler repeats the guard.
	mux.HandleFunc("POST /api/v1/scans", a.requireAuth(a.requirePipeline(a.handleCreateScan)))
	mux.HandleFunc("GET /api/v1/scans", a.requireAuth(a.requirePipeline(a.handleListScans)))
	mux.HandleFunc("GET /api/v1/scans/{id}", a.requireAuth(a.requirePipeline(a.handleGetScan)))
	mux.HandleFunc("GET /api/v1/overrides", a.requireAuth(a.requirePipeline(a.handleListOverrides)))
	mux.HandleFunc("POST /api/v1/overrides", a.requireAuth(a.requirePipeline(a.handleUpsertOverride)))
	// meta/search needs only an authed caller + a configured metadata client (no
	// store/scheduler), so it uses requireMeta rather than requirePipeline.
	mux.HandleFunc("GET /api/v1/meta/search", a.requireAuth(a.requireMeta(a.handleMetaSearch)))
	mux.HandleFunc("POST /api/v1/books", a.requireAuth(a.requirePipeline(a.handleCreateBooks)))
	mux.HandleFunc("GET /api/v1/books", a.requireAuth(a.requirePipeline(a.handleListBooks)))
	mux.HandleFunc("GET /api/v1/books/{id}", a.requireAuth(a.requirePipeline(a.handleGetBook)))
	mux.HandleFunc("GET /api/v1/books/{id}/sidecars", a.requireAuth(a.requirePipeline(a.handleBookSidecars)))
	mux.HandleFunc("GET /api/v1/books/{id}/events", a.requireAuth(a.requirePipeline(a.handleBookEvents)))
	mux.HandleFunc("GET /api/v1/books/{id}/contrib/core", a.requireAuth(a.requirePipeline(a.handleGetCoreProposal)))
	mux.HandleFunc("POST /api/v1/books/{id}/contribute/core", a.requireAuth(a.requirePipeline(a.handleContributeCore)))
	mux.HandleFunc("POST /api/v1/books/{id}/work", a.requireAuth(a.requirePipeline(a.handleSetWork)))
	mux.HandleFunc("GET /api/v1/books/{id}/export", a.requireAuth(a.requirePipeline(a.handleBookExport)))
	mux.HandleFunc("POST /api/v1/books/{id}/pause", a.requireAuth(a.requirePipeline(a.bookAction((*scheduler.Scheduler).Pause))))
	mux.HandleFunc("POST /api/v1/books/{id}/resume", a.requireAuth(a.requirePipeline(a.bookAction((*scheduler.Scheduler).Resume))))
	mux.HandleFunc("POST /api/v1/books/{id}/retry", a.requireAuth(a.requirePipeline(a.bookAction((*scheduler.Scheduler).Retry))))
	mux.HandleFunc("POST /api/v1/books/{id}/cancel", a.requireAuth(a.requirePipeline(a.bookAction((*scheduler.Scheduler).Cancel))))
	mux.HandleFunc("POST /api/v1/books/{id}/purge-scratch", a.requireAuth(a.requirePipeline(a.handlePurgeScratch)))
	mux.HandleFunc("DELETE /api/v1/books/{id}", a.requireAuth(a.requirePipeline(a.handleDeleteBook)))
	mux.HandleFunc("GET /api/v1/supervisor/status", a.requireAuth(a.requirePipeline(a.requireSupervisor(a.handleSupervisorStatus))))
	mux.HandleFunc("GET /api/v1/supervisor/incidents", a.requireAuth(a.requirePipeline(a.requireSupervisor(a.handleSupervisorIncidents))))
	mux.HandleFunc("GET /api/v1/supervisor/costs", a.requireAuth(a.requirePipeline(a.requireSupervisor(a.handleSupervisorCosts))))
	mux.HandleFunc("POST /api/v1/books/{id}/ask-supervisor", a.requireAuth(a.requirePipeline(a.requireSupervisor(a.handleAskSupervisor))))

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
