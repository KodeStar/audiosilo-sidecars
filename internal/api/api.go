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
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
)

// Deps are the collaborators the API needs.
type Deps struct {
	Auth    *auth.Manager
	Limiter *auth.RateLimiter
	Secrets secrets.Store
	Events  *events.Hub
	Version string
	DataDir string
	// Config is the loaded configuration. The API owns it after construction and
	// serializes reads/writes; Save persists mutations (e.g. cors_origins) back to
	// config.yaml.
	Config config.Config
	Save   func(config.Config) error
}

// API is the HTTP transport.
type API struct {
	auth    *auth.Manager
	limiter *auth.RateLimiter
	secrets secrets.Store
	events  *events.Hub
	version string
	dataDir string

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
		auth:    d.Auth,
		limiter: d.Limiter,
		secrets: d.Secrets,
		events:  d.Events,
		version: d.Version,
		dataDir: d.DataDir,
		cfg:     d.Config,
		save:    save,
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
