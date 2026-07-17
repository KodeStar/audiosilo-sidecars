package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// handleBookSidecars serves the metaserve-API-shaped preview of a book's contributed
// characters/recaps sidecars (the Done panel renders it with the vendored expressive
// components). The compose/flatten logic lives in internal/pipeline (injected as
// SidecarLoader so the api never imports pipeline); this handler is transport-only:
// it resolves the book, calls the loader, and maps ErrNoSidecars to 404.
func (a *API) handleBookSidecars(w http.ResponseWriter, r *http.Request) {
	b, ok := a.lookupBook(w, r)
	if !ok {
		return
	}
	if a.sidecarLoader == nil {
		writeError(w, http.StatusServiceUnavailable, "sidecar preview not available")
		return
	}
	raw, err := a.sidecarLoader(b.WorkDir)
	if errors.Is(err, ErrNoSidecars) {
		writeError(w, http.StatusNotFound, "no sidecars for this book yet")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read sidecars")
		return
	}
	writeJSON(w, http.StatusOK, raw)
}

// eventsDefaultLimit / eventsMaxLimit bound the per-book log page. A non-numeric or
// out-of-range limit clamps to this window rather than erroring, so a malformed
// query still returns a usable page.
const (
	eventsDefaultLimit = 100
	eventsMaxLimit     = 500
)

// loggedEventView is the wire shape of one durable-log row: the SSE-shared id, the
// timestamp, the event type, and the raw payload. book_id/hub_id are dropped (the
// book is the request's own id; the SSE hub id is an internal ordering key).
type loggedEventView struct {
	ID      int64           `json:"id"`
	TS      string          `json:"ts"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type bookEventsResponse struct {
	Events []loggedEventView `json:"events"`
}

// handleBookEvents returns the book's durable event backlog (newest first) from the
// store. The live feed is the SSE hub; this reads the persisted log so a reloaded
// Done/Running detail view can show history. limit defaults to 100 and clamps to
// 1..500; a non-numeric limit falls back to the default.
func (a *API) handleBookEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if _, err := a.store.GetBook(ctx, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "book not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read book")
		return
	}

	limit := eventsDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > eventsMaxLimit {
		limit = eventsMaxLimit
	}

	rows, err := a.store.ListEvents(ctx, id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read events")
		return
	}
	views := make([]loggedEventView, 0, len(rows))
	for _, e := range rows {
		views = append(views, loggedEventView{ID: e.ID, TS: e.TS, Type: e.Type, Payload: e.Payload})
	}
	writeJSON(w, http.StatusOK, bookEventsResponse{Events: views})
}
