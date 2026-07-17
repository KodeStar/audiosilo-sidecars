package api

import (
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-sidecars/internal/contrib"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// contributionRowView is the wire shape of one contribution row: the created
// issue/PR (number/url), the intake bot PR it produced (pr_number/pr_url, issue
// mode), and its lifecycle status/note. Used both in bookDetail.contributions and
// as the POST /contribute/core response.
type contributionRowView struct {
	Kind      string `json:"kind"`
	Mode      string `json:"mode"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	URL       string `json:"url"`
	PRNumber  int    `json:"pr_number"`
	PRURL     string `json:"pr_url"`
	Status    string `json:"status"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toContributionRowView(c store.Contribution) contributionRowView {
	return contributionRowView{
		Kind: c.Kind, Mode: c.Mode, Repo: c.Repo, Number: c.Number, URL: c.URL,
		PRNumber: c.PRNumber, PRURL: c.PRURL, Status: c.Status, Note: c.Note,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// handleGetCoreProposal serves a book's prefilled contrib/core_proposal.json so the
// UI can prefill the work-proposal form. The compose/read logic lives in pipeline
// (injected as CoreProposalLoader so api never imports it); this handler resolves
// the book and maps ErrNoCoreProposal to 404.
func (a *API) handleGetCoreProposal(w http.ResponseWriter, r *http.Request) {
	b, ok := a.lookupBook(w, r)
	if !ok {
		return
	}
	if a.coreProposalLoader == nil {
		writeError(w, http.StatusServiceUnavailable, "core proposal not available")
		return
	}
	raw, err := a.coreProposalLoader(b.WorkDir)
	if errors.Is(err, ErrNoCoreProposal) {
		writeError(w, http.StatusNotFound, "no work proposal for this book")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read work proposal")
		return
	}
	writeJSON(w, http.StatusOK, raw)
}

// handleContributeCore submits a completed work (add-work) proposal for a book that
// is parked awaiting one. It gates on the book being parked core_needed (409
// otherwise), validates the proposal (400), then delegates to the contrib service,
// which opens the intake issue, records the row, and flips the park to core_pending.
func (a *API) handleContributeCore(w http.ResponseWriter, r *http.Request) {
	b, ok := a.lookupBook(w, r)
	if !ok {
		return
	}
	if a.contrib == nil {
		writeError(w, http.StatusServiceUnavailable, "contribution not available")
		return
	}
	if !parkedWith(b, state.ParkCoreNeeded) {
		writeError(w, http.StatusConflict, "book is not awaiting a work proposal (park core_needed)")
		return
	}
	var p contrib.CoreProposal
	if !decodeJSON(w, r, &p) {
		return
	}
	// Validate here so the message surfaces as a 400 (SubmitCore re-checks defensively).
	if err := p.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	row, err := a.contrib.SubmitCore(r.Context(), b, p)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, toContributionRowView(row))
	case errors.Is(err, contrib.ErrNoCredential):
		writeError(w, http.StatusConflict,
			"no GitHub credential - add a personal access token in Settings or run `gh auth login`, then retry")
	case errors.Is(err, contrib.ErrNotAwaitingCore):
		writeError(w, http.StatusConflict, "book is not awaiting a work proposal")
	case isRateLimit(err):
		writeError(w, http.StatusBadGateway, "GitHub rate limit reached - try again later")
	default:
		writeError(w, http.StatusInternalServerError, "could not submit work proposal")
	}
}

// setWorkRequest is the POST /books/{id}/work body.
type setWorkRequest struct {
	WorkID string `json:"work_id"`
}

// handleSetWork records a manually-supplied upstream work slug on a book and, when
// the book was parked awaiting a work, re-admits it. The slug is validated for shape
// and existence (via the contrib service); a disabled metadata service accepts the
// shape alone.
func (a *API) handleSetWork(w http.ResponseWriter, r *http.Request) {
	b, ok := a.lookupBook(w, r)
	if !ok {
		return
	}
	if a.contrib == nil {
		writeError(w, http.StatusServiceUnavailable, "contribution not available")
		return
	}
	var req setWorkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	err := a.contrib.SetWork(r.Context(), b, req.WorkID)
	switch {
	case err == nil:
		// Re-read: SetWork may have set work_id and re-admitted (cleared) the book.
		nb, gerr := a.store.GetBook(r.Context(), b.ID)
		if gerr != nil {
			writeError(w, http.StatusInternalServerError, "could not read book")
			return
		}
		contribRows, _ := a.store.ListContributionsByBook(r.Context(), b.ID)
		writeJSON(w, http.StatusOK, a.bookToView(r.Context(), nb, contribRows))
	case errors.Is(err, contrib.ErrInvalidSlug):
		writeError(w, http.StatusBadRequest, "invalid work id")
	case errors.Is(err, contrib.ErrWorkNotFound):
		writeError(w, http.StatusBadRequest, "work not found upstream")
	default:
		writeError(w, http.StatusBadGateway, "could not verify work upstream")
	}
}

// handleBookExport streams a zip of a book's sidecars in the meta repo's layout as a
// download. The file set is fixed and the slug validated in pipeline, so no
// user-supplied path reaches the filesystem.
func (a *API) handleBookExport(w http.ResponseWriter, r *http.Request) {
	b, ok := a.lookupBook(w, r)
	if !ok {
		return
	}
	if a.exportArchive == nil {
		writeError(w, http.StatusServiceUnavailable, "export not available")
		return
	}
	data, filename, err := a.exportArchive(b)
	if errors.Is(err, ErrNoSidecars) {
		writeError(w, http.StatusNotFound, "no sidecars for this book yet")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build export")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// lookupBook parses the {id} path value and loads the book, writing a 400/404/500 on
// failure. It centralizes the resolve-book preamble the contribution handlers share.
func (a *API) lookupBook(w http.ResponseWriter, r *http.Request) (store.Book, bool) {
	id, ok := parseID(w, r)
	if !ok {
		return store.Book{}, false
	}
	b, err := a.store.GetBook(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "book not found")
		return store.Book{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read book")
		return store.Book{}, false
	}
	return b, true
}

// parkedWith reports whether a book is parked (needs_attention) with the given code.
// It defers to state.IsParkedWith so the api and contrib share one park-code predicate.
func parkedWith(b store.Book, code state.ParkCode) bool {
	return state.IsParkedWith(b.Status, b.ParkCode, code)
}

// isRateLimit reports whether err is (or wraps) a GitHub rate-limit error.
func isRateLimit(err error) bool {
	var rl *contrib.RateLimitError
	return errors.As(err, &rl)
}
