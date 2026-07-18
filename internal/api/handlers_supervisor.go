package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/supervisor"
)

func (a *API) handleSupervisorStatus(w http.ResponseWriter, r *http.Request) {
	if a.supervisor == nil {
		writeError(w, http.StatusServiceUnavailable, "supervisor is not wired")
		return
	}
	writeJSON(w, http.StatusOK, a.supervisor.Status())
}

func (a *API) handleSupervisorIncidents(w http.ResponseWriter, r *http.Request) {
	if a.supervisor == nil {
		writeError(w, http.StatusServiceUnavailable, "supervisor is not wired")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	runs, err := a.supervisor.Recent(r.Context(), strings.TrimSpace(r.URL.Query().Get("batch_id")), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read supervisor incidents")
		return
	}
	if runs == nil {
		runs = []store.SupervisorRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": runs})
}

func (a *API) handleSupervisorCosts(w http.ResponseWriter, r *http.Request) {
	if a.supervisor == nil {
		writeError(w, http.StatusServiceUnavailable, "supervisor is not wired")
		return
	}
	batch := strings.TrimSpace(r.URL.Query().Get("batch_id"))
	if batch == "" {
		writeError(w, http.StatusBadRequest, "batch_id is required")
		return
	}
	costs, err := a.supervisor.Costs(r.Context(), batch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read supervisor costs")
		return
	}
	writeJSON(w, http.StatusOK, costs)
}

func (a *API) handleAskSupervisor(w http.ResponseWriter, r *http.Request) {
	if a.supervisor == nil {
		writeError(w, http.StatusServiceUnavailable, "supervisor is not wired")
		return
	}
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	run, err := a.supervisor.Ask(r.Context(), id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, run)
	case errors.Is(err, supervisor.ErrModelDisabled):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "book not found")
	default:
		writeError(w, http.StatusInternalServerError, "supervisor request failed")
	}
}
