package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/kodestar/audiosilo-sidecars/internal/auth"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
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
	// ScratchBytes is the daemon-total on-disk scratch (the sum of every book's
	// work dir under <data>/work), the disk gauge the UI shows.
	ScratchBytes int64 `json:"scratch_bytes"`
}

// tabs is the static tab list. Library/Running/Settings are functional as of
// M1 (Running in its minimal live-list form); Done lands with the M6 board.
var tabs = []tabInfo{
	{ID: "library", Label: "Library", Status: "ready"},
	{ID: "running", Label: "Running", Status: "ready"},
	{ID: "done", Label: "Done", Status: "planned"},
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
	writeJSON(w, http.StatusOK, systemResponse{
		Version:      a.version,
		DataDir:      a.dataDir,
		Listen:       cfg.Listen,
		Tabs:         tabs,
		Tools:        toolsInfo{FFmpeg: a.ffmpeg, FFprobe: a.ffprobe},
		ASR:          a.asr,
		ScratchBytes: scratchTotal,
	})
}

// --- settings ---

type asrView struct {
	Backend string `json:"backend"`
	Device  string `json:"device"`
}

type agentView struct {
	Backend     string `json:"backend"`
	Concurrency int    `json:"concurrency"`
}

type settingsResponse struct {
	Listen      string          `json:"listen"`
	CORSOrigins []string        `json:"cors_origins"`
	Secrets     map[string]bool `json:"secrets"`
	ASR         asrView         `json:"asr"`
	Agent       agentView       `json:"agent"`
}

// settingsView composes the read model. Secrets are presence booleans ONLY - the
// values never leave the daemon.
func (a *API) settingsView() (settingsResponse, error) {
	cfg := a.snapshot()
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
		ASR:         asrView{Backend: cfg.ASR.Backend, Device: cfg.ASR.Device},
		Agent:       agentView{Backend: cfg.Agent.Backend, Concurrency: cfg.Agent.Concurrency},
	}, nil
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
type settingsUpdate struct {
	CORSOrigins *[]string         `json:"cors_origins"`
	Secrets     map[string]string `json:"secrets"`
}

func (a *API) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsUpdate
	if !decodeJSON(w, r, &req) {
		return
	}

	// Apply cors_origins (validated) and persist config.
	if req.CORSOrigins != nil {
		next := a.snapshot()
		origins := *req.CORSOrigins
		if origins == nil {
			origins = []string{}
		}
		next.CORSOrigins = origins
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
