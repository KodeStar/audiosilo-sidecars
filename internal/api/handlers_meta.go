package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// requireMeta wraps a handler that needs a live community-metadata client,
// returning 503 when metadata lookup is unconfigured/disabled (composed with
// requireAuth at route registration).
func (a *API) requireMeta(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.meta == nil || !a.meta.Enabled() {
			writeError(w, http.StatusServiceUnavailable, "metadata lookup is not configured")
			return
		}
		next(w, r)
	}
}

// --- candidate overrides ---

// overrideDTO is the wire shape of a persisted candidate override.
type overrideDTO struct {
	SourcePath string `json:"source_path"`
	Hidden     bool   `json:"hidden"`
	WorkID     string `json:"work_id,omitempty"`
	WorkTitle  string `json:"work_title,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

func overrideToDTO(o store.Override) overrideDTO {
	return overrideDTO{
		SourcePath: o.SourcePath, Hidden: o.Hidden,
		WorkID: o.WorkID, WorkTitle: o.WorkTitle, UpdatedAt: o.UpdatedAt,
	}
}

func storedOverrideToDTO(o metaops.StoredOverride) overrideDTO {
	return overrideDTO{
		SourcePath: o.SourcePath, Hidden: o.Hidden,
		WorkID: o.WorkID, WorkTitle: o.WorkTitle, UpdatedAt: o.UpdatedAt,
	}
}

type listOverridesResponse struct {
	Overrides []overrideDTO `json:"overrides"`
}

// handleListOverrides returns all persisted candidate overrides so the Library
// UI can render hidden/manually-matched state after a reload.
func (a *API) handleListOverrides(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListOverrides(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list overrides")
		return
	}
	out := make([]overrideDTO, 0, len(list))
	for _, o := range list {
		out = append(out, overrideToDTO(o))
	}
	writeJSON(w, http.StatusOK, listOverridesResponse{Overrides: out})
}

type upsertOverrideRequest struct {
	SourcePath string `json:"source_path"`
	Hidden     bool   `json:"hidden"`
	WorkID     string `json:"work_id"`
}

type upsertOverrideResponse struct {
	Override overrideDTO       `json:"override"`
	Coverage *metaops.Coverage `json:"coverage"`
}

// handleUpsertOverride writes the full desired state for a source path: hide
// and/or manually match to a work. The orchestration (allow-list check, manual-
// match resolution, persistence, and the in-memory scan overlay) lives in
// metaops.OverrideService; this handler is pure transport - decode, call, map
// the error taxonomy to a status, encode.
func (a *API) handleUpsertOverride(w http.ResponseWriter, r *http.Request) {
	var req upsertOverrideRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := a.overrides.Upsert(r.Context(), metaops.OverrideRequest{
		SourcePath: req.SourcePath, Hidden: req.Hidden, WorkID: req.WorkID,
	}, a.snapshot().LibraryRoots)
	switch {
	case errors.Is(err, metaops.ErrNoSourcePath):
		writeError(w, http.StatusBadRequest, "source_path is required")
		return
	case errors.Is(err, metaops.ErrPathNotAllowed):
		writeError(w, http.StatusForbidden, "path is outside the configured library roots")
		return
	case errors.Is(err, metaops.ErrWorkNotFound):
		writeError(w, http.StatusBadRequest, "no metadata work matches that id")
		return
	case errors.Is(err, metaops.ErrDisabled):
		writeError(w, http.StatusServiceUnavailable, "metadata lookup is not configured")
		return
	case errors.Is(err, metaops.ErrUpstream):
		writeError(w, http.StatusBadGateway, "metadata service is unreachable")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not save override")
		return
	}
	writeJSON(w, http.StatusOK, upsertOverrideResponse{
		Override: storedOverrideToDTO(res.Override),
		Coverage: res.Coverage,
	})
}

// --- metadata search proxy ---

type metaSearchResponse struct {
	Results []metaops.WorkSearchResult `json:"results"`
}

// handleMetaSearch proxies a free-text query to the metadata search endpoint,
// returning work hits for the manual-match picker. requireMeta guarantees a live
// client; an upstream failure maps to 502.
func (a *API) handleMetaSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	// metaops.SearchLimit is the single source of truth for both the default and
	// the max: an explicit limit is honored up to and including it (a client asking
	// for exactly the max gets the max, not the max-1 an exclusive bound would give).
	limit := metaops.SearchLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= limit {
			limit = n
		}
	}
	res, err := a.meta.SearchWorks(r.Context(), q, limit)
	if err != nil {
		if errors.Is(err, metaops.ErrDisabled) {
			writeError(w, http.StatusServiceUnavailable, "metadata lookup is not configured")
			return
		}
		writeError(w, http.StatusBadGateway, "metadata service is unreachable")
		return
	}
	writeJSON(w, http.StatusOK, metaSearchResponse{Results: res})
}
