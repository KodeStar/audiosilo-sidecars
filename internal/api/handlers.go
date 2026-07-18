package api

import (
	"errors"
	"maps"
	"net/http"
	"strconv"

	"github.com/kodestar/audiosilo-sidecars/internal/auth"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
	"github.com/kodestar/audiosilo-sidecars/internal/supervisor"
)

// --- auth ---

type loginRequest struct {
	Password string `json:"password"`
}

type loginResponse struct {
	Token string `json:"token"`
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, slow down")
		return
	}
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token, err := a.auth.Login(req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCreds) || errors.Is(err, auth.ErrNoAdmin) {
			writeError(w, http.StatusUnauthorized, "invalid password")
			return
		}
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{Token: token})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := a.auth.Logout(bearerToken(r)); err != nil {
		writeError(w, http.StatusInternalServerError, "logout failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type passwordRequest struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (a *API) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	err := a.auth.ChangePassword(req.Current, req.New)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrInvalidCreds):
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
	case errors.Is(err, auth.ErrPasswordTooShort):
		writeError(w, http.StatusBadRequest, "new password must be at least "+strconv.Itoa(auth.MinPasswordLen)+" characters")
	default:
		writeError(w, http.StatusInternalServerError, "could not change password")
	}
}

// --- system ---

type tabInfo struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"` // "ready" | "planned"
}

// toolsInfo surfaces the media-tool paths resolved at startup (empty when a tool
// could not be located). It is read-only diagnostic info, not a secret.
type toolsInfo struct {
	FFmpeg  string `json:"ffmpeg"`
	FFprobe string `json:"ffprobe"`
}

type systemResponse struct {
	Version string    `json:"version"`
	DataDir string    `json:"data_dir"`
	Listen  string    `json:"listen"`
	Tabs    []tabInfo `json:"tabs"`
	Tools   toolsInfo `json:"tools"`
	// ASR is the resolved speech-recognition backend capability (whether ASR will
	// run and on what device).
	ASR ASRInfo `json:"asr"`
	// Agent is the resolved agent-runner capability (which backend runs the agent
	// stages and whether it is usable).
	Agent AgentInfo `json:"agent"`
	// ScratchBytes is the daemon-total on-disk scratch (the sum of every book's
	// work dir under <data>/work), the disk gauge the UI shows.
	ScratchBytes int64              `json:"scratch_bytes"`
	Supervisor   *supervisor.Status `json:"supervisor,omitempty"`
}

// tabs is the static tab list. Library/Running/Settings are functional as of
// M1 (Running in its minimal live-list form); Done lands with the M6 board.
var tabs = []tabInfo{
	{ID: "library", Label: "Library", Status: "ready"},
	{ID: "running", Label: "Running", Status: "ready"},
	{ID: "done", Label: "Done", Status: "ready"},
	{ID: "settings", Label: "Settings", Status: "ready"},
}

func (a *API) handleSystem(w http.ResponseWriter, r *http.Request) {
	cfg := a.snapshot()
	// The daemon-total disk gauge is served from the accounted column (no walk).
	// The store is absent in the M0 auth-only wiring, so degrade to 0 there.
	var scratchTotal int64
	if a.store != nil {
		scratchTotal, _ = a.store.SumScratchBytes(r.Context())
	}
	// Prefer the executor's live values (a stage may have re-detected an ASR backend
	// or a media tool after startup); fall back to the boot-time snapshot.
	asrInfo, ffmpeg, ffprobe := a.asr, a.ffmpeg, a.ffprobe
	if a.liveStatus != nil {
		asrInfo, ffmpeg, ffprobe = a.liveStatus()
	}
	// Agent availability is injected like ASR; a nil provider yields a zero
	// AgentInfo (no backend, unavailable).
	var agentInfo AgentInfo
	if a.agentStatus != nil {
		agentInfo = a.agentStatus()
	}
	var supervisorInfo *supervisor.Status
	if a.supervisor != nil {
		v := a.supervisor.Status()
		supervisorInfo = &v
	}
	writeJSON(w, http.StatusOK, systemResponse{
		Version:      a.version,
		DataDir:      a.dataDir,
		Listen:       cfg.Listen,
		Tabs:         tabs,
		Tools:        toolsInfo{FFmpeg: ffmpeg, FFprobe: ffprobe},
		ASR:          asrInfo,
		Agent:        agentInfo,
		ScratchBytes: scratchTotal,
		Supervisor:   supervisorInfo,
	})
}

// --- settings ---

type asrView struct {
	Backend string `json:"backend"`
}

type agentView struct {
	Backend                        string            `json:"backend"`
	Concurrency                    int               `json:"concurrency"` // compatible effective/legacy cap
	QueueConcurrency               int               `json:"queue_concurrency"`
	MaxAgentsPerBook               int               `json:"max_agents_per_book"`
	EffectiveGlobalInvocationLimit int               `json:"effective_global_invocation_limit"`
	LegacyConcurrency              bool              `json:"legacy_concurrency"`
	TimeoutMinutes                 int               `json:"timeout_minutes"`
	BookBudgetUSD                  float64           `json:"book_budget_usd"`
	ClaudeModels                   map[string]string `json:"claude_models"`
	OpenAIModels                   map[string]string `json:"openai_models"`
}

type contributionView struct {
	Mode        string `json:"mode"`
	Repo        string `json:"repo"`
	AutoPurge   bool   `json:"auto_purge"`
	PollMinutes int    `json:"poll_minutes"`
}

type supervisorView struct {
	Enabled               bool `json:"enabled"`
	AutomaticActions      bool `json:"automatic_actions"`
	ModelAssisted         bool `json:"model_assisted"`
	ModelAutomaticActions bool `json:"model_automatic_actions"`
	AllowBackendFailover  bool `json:"allow_backend_failover"`
}

type settingsResponse struct {
	Listen       string           `json:"listen"`
	CORSOrigins  []string         `json:"cors_origins"`
	Secrets      map[string]bool  `json:"secrets"`
	ASR          asrView          `json:"asr"`
	Agent        agentView        `json:"agent"`
	Contribution contributionView `json:"contribution"`
	Supervisor   supervisorView   `json:"supervisor"`
}

// settingsView composes the read model. Secrets are presence booleans ONLY - the
// values never leave the daemon.
func (a *API) settingsView() (settingsResponse, error) {
	cfg := a.snapshot()
	capacity := cfg.Agent.Capacity()
	pres := make(map[string]bool, len(secrets.Names()))
	for _, name := range secrets.Names() {
		p, err := a.secrets.Present(name)
		if err != nil {
			return settingsResponse{}, err
		}
		pres[name] = p
	}
	origins := cfg.CORSOrigins
	if origins == nil {
		origins = []string{}
	}
	return settingsResponse{
		Listen:      cfg.Listen,
		CORSOrigins: origins,
		Secrets:     pres,
		ASR:         asrView{Backend: cfg.ASR.Backend},
		Agent: agentView{
			Backend:                        cfg.Agent.Backend,
			Concurrency:                    capacity.QueueConcurrency,
			QueueConcurrency:               capacity.QueueConcurrency,
			MaxAgentsPerBook:               capacity.MaxAgentsPerBook,
			EffectiveGlobalInvocationLimit: capacity.GlobalInvocations,
			LegacyConcurrency:              capacity.Legacy,
			TimeoutMinutes:                 cfg.Agent.TimeoutMinutes,
			BookBudgetUSD:                  cfg.Agent.BookBudgetUSD,
			ClaudeModels:                   copyStringMap(cfg.Agent.Claude),
			OpenAIModels:                   copyStringMap(cfg.Agent.OpenAI),
		},
		Contribution: contributionView{
			Mode:        cfg.Contribution.Mode,
			Repo:        cfg.Contribution.Repo,
			AutoPurge:   cfg.Contribution.AutoPurge,
			PollMinutes: cfg.Contribution.PollMinutes,
		},
		Supervisor: supervisorView{Enabled: cfg.Supervisor.Enabled, AutomaticActions: cfg.Supervisor.AutomaticActions,
			ModelAssisted: cfg.Supervisor.ModelAssisted, ModelAutomaticActions: cfg.Supervisor.ModelAutomaticActions,
			AllowBackendFailover: cfg.Supervisor.AllowBackendFailover},
	}, nil
}

// copyStringMap returns a defensive copy of m, never nil - the settings view emits
// {} rather than null for the model maps so the UI can index them unconditionally.
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}

func (a *API) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	view, err := a.settingsView()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read settings")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// settingsUpdate is the PUT body. All fields are optional; an omitted field is
// left untouched. Secret values are write-only: a non-empty value sets the
// secret, an empty string clears it, and the response never echoes them.
//
// Agent changes are persisted to config.yaml but, like the ASR backend, only take
// effect on a daemon RESTART (the runner is resolved once at startup) - nothing
// under Agent is live. The response body is the fresh GET view.
type settingsUpdate struct {
	CORSOrigins  *[]string           `json:"cors_origins"`
	Secrets      map[string]string   `json:"secrets"`
	Agent        *agentUpdate        `json:"agent"`
	Contribution *contributionUpdate `json:"contribution"`
	Supervisor   *supervisorUpdate   `json:"supervisor"`
}

type supervisorUpdate struct {
	Enabled               *bool `json:"enabled"`
	AutomaticActions      *bool `json:"automatic_actions"`
	ModelAssisted         *bool `json:"model_assisted"`
	ModelAutomaticActions *bool `json:"model_automatic_actions"`
	AllowBackendFailover  *bool `json:"allow_backend_failover"`
}

func applySupervisorUpdate(cfg *config.SupervisorConfig, u *supervisorUpdate) {
	if u.Enabled != nil {
		cfg.Enabled = *u.Enabled
	}
	if u.AutomaticActions != nil {
		cfg.AutomaticActions = *u.AutomaticActions
	}
	if u.ModelAssisted != nil {
		cfg.ModelAssisted = *u.ModelAssisted
	}
	if u.ModelAutomaticActions != nil {
		cfg.ModelAutomaticActions = *u.ModelAutomaticActions
	}
	if u.AllowBackendFailover != nil {
		cfg.AllowBackendFailover = *u.AllowBackendFailover
	}
}

// agentUpdate carries the optional agent-config mutations. Scalar fields are
// pointers (nil = leave unchanged); the model maps replace the corresponding config
// map wholesale when present (a nil map = leave unchanged). config.Validate rejects
// a bad backend, a sub-1 timeout, or a non-agent-stage model key.
type agentUpdate struct {
	Backend          *string           `json:"backend"`
	Concurrency      *int              `json:"concurrency"`
	QueueConcurrency *int              `json:"queue_concurrency"`
	MaxAgentsPerBook *int              `json:"max_agents_per_book"`
	TimeoutMinutes   *int              `json:"timeout_minutes"`
	BookBudgetUSD    *float64          `json:"book_budget_usd"`
	Claude           map[string]string `json:"claude_models"`
	OpenAI           map[string]string `json:"openai_models"`
}

// contributionUpdate carries the optional contribution-config mutations. Each field
// is a pointer (nil = leave unchanged). config.Validate rejects a bad mode, a
// malformed repo, or a sub-1 poll interval. Like the agent config, changes persist to
// config.yaml but take effect only on a daemon RESTART.
type contributionUpdate struct {
	Mode        *string `json:"mode"`
	Repo        *string `json:"repo"`
	AutoPurge   *bool   `json:"auto_purge"`
	PollMinutes *int    `json:"poll_minutes"`
}

// applyContributionUpdate overlays u onto cfg in place.
func applyContributionUpdate(cfg *config.ContributionConfig, u *contributionUpdate) {
	if u.Mode != nil {
		cfg.Mode = *u.Mode
	}
	if u.Repo != nil {
		cfg.Repo = *u.Repo
	}
	if u.AutoPurge != nil {
		cfg.AutoPurge = *u.AutoPurge
	}
	if u.PollMinutes != nil {
		cfg.PollMinutes = *u.PollMinutes
	}
}

// applyAgentUpdate overlays u onto cfg in place.
func applyAgentUpdate(cfg *config.AgentConfig, u *agentUpdate) {
	if u.Backend != nil {
		cfg.Backend = *u.Backend
	}
	if u.Concurrency != nil {
		cfg.Concurrency = *u.Concurrency
		cfg.QueueConcurrency, cfg.MaxAgentsPerBook = 0, 0
	}
	if u.QueueConcurrency != nil || u.MaxAgentsPerBook != nil {
		previous := cfg.Capacity()
		cfg.Concurrency = 0
		if cfg.QueueConcurrency == 0 {
			cfg.QueueConcurrency = previous.QueueConcurrency
		}
		if cfg.MaxAgentsPerBook == 0 {
			cfg.MaxAgentsPerBook = previous.MaxAgentsPerBook
		}
		if u.QueueConcurrency != nil {
			cfg.QueueConcurrency = *u.QueueConcurrency
		}
		if u.MaxAgentsPerBook != nil {
			cfg.MaxAgentsPerBook = *u.MaxAgentsPerBook
		}
	}
	if u.TimeoutMinutes != nil {
		cfg.TimeoutMinutes = *u.TimeoutMinutes
	}
	if u.BookBudgetUSD != nil {
		cfg.BookBudgetUSD = *u.BookBudgetUSD
	}
	if u.Claude != nil {
		cfg.Claude = u.Claude
	}
	if u.OpenAI != nil {
		cfg.OpenAI = u.OpenAI
	}
}

func (a *API) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsUpdate
	if !decodeJSON(w, r, &req) {
		return
	}

	// Apply config-backed fields (cors_origins + agent) onto one copy, validate the
	// whole thing, then persist. Agent maps replace wholesale when present; a nil map
	// in the update leaves the existing map untouched.
	if req.CORSOrigins != nil || req.Agent != nil || req.Contribution != nil || req.Supervisor != nil {
		next := a.snapshot()
		if req.CORSOrigins != nil {
			origins := *req.CORSOrigins
			if origins == nil {
				origins = []string{}
			}
			next.CORSOrigins = origins
		}
		if req.Agent != nil {
			applyAgentUpdate(&next.Agent, req.Agent)
		}
		if req.Contribution != nil {
			applyContributionUpdate(&next.Contribution, req.Contribution)
		}
		if req.Supervisor != nil {
			applySupervisorUpdate(&next.Supervisor, req.Supervisor)
		}
		if err := next.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.applyConfig(next); err != nil {
			writeError(w, http.StatusInternalServerError, "could not persist settings")
			return
		}
	}

	// Route secret values to the secret store (write-only). Unknown names are
	// ignored so a client cannot probe arbitrary keys.
	if req.Secrets != nil {
		recognized := recognizedSecrets()
		for name, value := range req.Secrets {
			if !recognized[name] {
				continue
			}
			var err error
			if value == "" {
				err = a.secrets.Delete(name)
			} else {
				err = a.secrets.Set(name, value)
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, "could not store secret")
				return
			}
		}
	}

	view, err := a.settingsView()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read settings")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// applyConfig swaps in next and persists it.
func (a *API) applyConfig(next config.Config) error {
	a.mu.Lock()
	a.cfg = next
	save := a.save
	a.mu.Unlock()
	return save(next)
}

func recognizedSecrets() map[string]bool {
	m := make(map[string]bool, len(secrets.Names()))
	for _, n := range secrets.Names() {
		m[n] = true
	}
	return m
}

// --- events (SSE) ---

func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	// SSE auth: EventSource cannot set an Authorization header, so accept the
	// token from the query string here (this endpoint only).
	token := bearerToken(r)
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	ok, err := a.auth.Resolve(token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "auth error")
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)

	replay, sub := a.events.Subscribe(lastEventID(r))
	defer sub.Close()

	// Replay missed events, then an immediate heartbeat so the UI liveness dot
	// lights up at once rather than after the first interval.
	for _, ev := range replay {
		if err := ev.WriteSSE(w); err != nil {
			return
		}
	}
	if err := events.NewHeartbeat().WriteSSE(w); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return // evicted
			}
			if err := ev.WriteSSE(w); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// lastEventID reads the resume position from the Last-Event-ID header (set
// automatically by the browser on reconnect) or the lastEventId query fallback.
func lastEventID(r *http.Request) uint64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("lastEventId")
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return id
}
