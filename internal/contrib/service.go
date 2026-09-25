package contrib

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// ErrInvalidSlug is returned by SetWork when the supplied work id is not a valid
// meta slug. The API maps it to 400.
var ErrInvalidSlug = errors.New("contrib: invalid work id")

// ReleaseWaitMsg parks a book whose add-work PR merged but whose work no published
// data release holds yet (the community intake would refuse its sidecars); the
// poller re-admits it once the work is live.
const ReleaseWaitMsg = "the work proposal merged - waiting for the next data release to publish it; the book resumes automatically"

// ErrWorkNotFound signals that a work id does not exist upstream. The injected
// ResolveWork translates metaops.ErrWorkNotFound into this package-local sentinel
// (so contrib need not import metaops), and SetWork/the API surface it as a 400.
var ErrWorkNotFound = errors.New("contrib: work not found upstream")

// ContribUpdate is the SSE `contrib.update` payload published on every
// contribution row change, including actionable-note changes (by the poller),
// and on a core submit. The server
// marshals it through the event hub; contrib itself never imports events.
type ContribUpdate struct {
	BookID int64  `json:"book_id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	URL    string `json:"url,omitempty"`
}

// ServiceDeps are the injected collaborators shared by the core-submit endpoint
// logic and the intake poller. contrib depends only on store/state (+ the meta
// module's slug rules); the scheduler and event hub are reached through the
// Readmit/Publish function seams so contrib imports neither.
type ServiceDeps struct {
	// DB is the scheduling/state store (books + contribution rows).
	DB *store.DB
	// CoreRepo is the metadata database's CC0 core repository ("owner/name"), where
	// SubmitCore opens add-work issues. (Sidecars go to the community repository;
	// the pipeline's contributing stage opens those, and the poller follows every
	// row at the repository it records.)
	CoreRepo string
	// BaseURL is the GitHub REST base (empty defaults to api.github.com); tests
	// point it at an httptest server.
	BaseURL string
	// Tokens resolves a GitHub credential (PAT then `gh auth token`). The poller
	// tolerates no credential (public reads are unauthenticated); a core submit
	// requires one.
	Tokens TokenResolver
	// Publish fans a contribution-row change out as an SSE `contrib.update`.
	Publish func(ContribUpdate)
	// Readmit re-admits a parked book to the scheduler (scheduler.Retry): used when
	// a core PR merges (poller) or the work is set manually (SetWork).
	Readmit func(ctx context.Context, id int64) error
	// ResolveWork checks a work id is LIVE in the published catalogue and returns
	// the slug it is live under: the id itself, or - for a slug a merge retired -
	// the survivor the catalogue redirects it to. It returns (workID, nil) when the
	// metadata service is disabled (accept the slug shape alone), ErrWorkNotFound
	// when no published release holds the work, and any other error for a
	// transport failure (a transient 502). nil skips verification entirely.
	ResolveWork func(ctx context.Context, workID string) (string, error)
	// CorePendingMsg is the park message stamped when a core proposal is submitted
	// (the pipeline owns the canonical string; the server injects it here so contrib
	// does not import pipeline).
	CorePendingMsg string
	// Log is optional; poll failures are logged and swallowed.
	Log *slog.Logger
}

// TokenResolver resolves a GitHub credential. *TokenSource satisfies it; tests
// inject a deterministic resolver.
type TokenResolver interface {
	Resolve(ctx context.Context) (token, from string, err error)
}

// Service carries the M7 contribution seams shared by the API core-submit
// endpoint and the intake poller.
type Service struct {
	deps ServiceDeps
	now  func() time.Time // clock seam for age-based poller decisions

	// mu guards bookLocks; bookLocks holds one mutex per book id so concurrent
	// SubmitCore calls for the SAME book serialize (opening exactly one intake issue),
	// while different books proceed in parallel.
	mu        sync.Mutex
	bookLocks map[int64]*sync.Mutex
}

// NewService constructs a Service from its dependencies.
func NewService(deps ServiceDeps) *Service {
	return &Service{deps: deps, now: time.Now, bookLocks: make(map[int64]*sync.Mutex)}
}

// bookLock returns the per-book mutex, creating it on first use.
func (s *Service) bookLock(id int64) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.bookLocks[id]
	if !ok {
		m = &sync.Mutex{}
		s.bookLocks[id] = m
	}
	return m
}

// client builds a REST client for the resolved credential. token may be empty
// (public reads work unauthenticated); requireToken reports whether a credential
// is present, which the core submit requires.
func (s *Service) client(ctx context.Context) (cli *Client, requireToken bool) {
	token := ""
	if s.deps.Tokens != nil {
		if tok, _, err := s.deps.Tokens.Resolve(ctx); err == nil {
			token = tok
		}
	}
	base := s.deps.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	return NewClient(base, token), token != ""
}

// SubmitCore opens an add-work intake issue for a book whose work does not exist
// upstream, records a kind=core row (status submitted), and flips the book's park
// from core_needed to core_pending. The poller later resolves the real slug from
// the merged intake PR and re-admits the book.
//
// A missing GitHub credential returns ErrNoCredential (the API maps it to 409); a
// GitHub rate limit returns *RateLimitError (502). The returned Contribution is
// the persisted row.
func (s *Service) SubmitCore(ctx context.Context, book store.Book, p CoreProposal) (store.Contribution, error) {
	if err := p.Validate(); err != nil {
		return store.Contribution{}, err
	}

	// Serialize per book so a concurrent double-submit (two UI clicks / two tabs) opens
	// exactly one issue: the loser sees the winner's recorded row and reuses it.
	lock := s.bookLock(book.ID)
	lock.Lock()
	defer lock.Unlock()

	// Re-read under the lock: a concurrent or prior (partially-failed) submit may already
	// have opened the issue and/or flipped the park.
	fresh, err := s.deps.DB.GetBook(ctx, book.ID)
	if err != nil {
		return store.Contribution{}, fmt.Errorf("contrib: reload book: %w", err)
	}

	// Idempotent reuse: a core row that already carries a created issue is authoritative -
	// never open a second. Just ensure the park is flipped to core_pending (a prior submit
	// may have crashed between recording the row and flipping the park). Only an
	// IN-FLIGHT row is reused: a settled one (already_covered - the bot answered
	// duplicate - or closed) is followed by nothing, so reusing it would park the book
	// core_pending with no poll or release gate ever moving it again.
	existing, hasExisting := s.existingCoreRow(ctx, book.ID)
	if hasExisting && existing.URL != "" && coreRowInFlight(existing) {
		if err := s.ensureCorePending(ctx, fresh); err != nil {
			return store.Contribution{}, err
		}
		return existing, nil
	}

	// No reusable row: only open an issue while the book is still awaiting a work
	// (core_needed). If it has moved on with no recorded issue, refuse rather than open a
	// stray issue for a book that no longer needs one.
	if !state.IsParkedWith(fresh.Status, fresh.ParkCode, state.ParkCoreNeeded) {
		return store.Contribution{}, ErrNotAwaitingCore
	}

	cli, hasToken := s.client(ctx)
	if !hasToken {
		return store.Contribution{}, ErrNoCredential
	}

	title, body, labels := WorkIssue(p)
	issue, err := cli.CreateIssue(ctx, s.deps.CoreRepo, title, body, labels)
	if err != nil {
		return store.Contribution{}, err
	}

	// Persist the row IMMEDIATELY after CreateIssue succeeds, BEFORE the park flip, so a
	// crash between the two loses nothing: a resubmit finds the recorded issue and reuses
	// it (the idempotent-reuse branch above) instead of opening a duplicate.
	row, err := s.deps.DB.UpsertContribution(ctx, store.Contribution{
		BookID: book.ID,
		Kind:   store.ContribKindCore,
		Mode:   store.ContribModeIssue,
		Repo:   s.deps.CoreRepo,
		Number: issue.Number,
		URL:    issue.URL,
		Status: store.ContribStatusSubmitted,
	})
	if err != nil {
		return store.Contribution{}, fmt.Errorf("contrib: record core row: %w", err)
	}
	// The upsert keeps a replaced row's intake-PR pointer: clear it, or the poller
	// would follow the settled row's old PR instead of the new issue.
	if hasExisting && (row.PRNumber != 0 || row.PRURL != "") {
		if err := s.deps.DB.SetContributionStatus(ctx, row.ID, row.Status, 0, "", row.Note); err != nil {
			return store.Contribution{}, fmt.Errorf("contrib: reset core row: %w", err)
		}
		row.PRNumber, row.PRURL = 0, ""
	}

	// Flip the park core_needed -> core_pending (state stays contributing, status
	// stays needs_attention). SetBookStatus preserves the pipeline state.
	if err := s.ensureCorePending(ctx, fresh); err != nil {
		return store.Contribution{}, err
	}

	s.publish(book.ID, store.ContribKindCore, store.ContribStatusSubmitted, issue.URL)
	return row, nil
}

// ErrNotAwaitingCore is returned by SubmitCore when the book is neither awaiting a work
// (core_needed) nor already carries a recorded core issue - so there is nothing to
// submit and opening one would be spurious. The API maps it to 409.
var ErrNotAwaitingCore = errors.New("contrib: book is not awaiting a work proposal")

// coreRowInFlight reports whether a core row is still followed by the poller
// (submitted / pr_open) or the release gate (merged).
func coreRowInFlight(r store.Contribution) bool {
	switch r.Status {
	case store.ContribStatusSubmitted, store.ContribStatusPROpen, store.ContribStatusMerged:
		return true
	}
	return false
}

// existingCoreRow reads the book's kind=core contribution row, if any.
func (s *Service) existingCoreRow(ctx context.Context, bookID int64) (store.Contribution, bool) {
	rows, err := s.deps.DB.ListContributionsByBook(ctx, bookID)
	if err != nil {
		return store.Contribution{}, false
	}
	return findCore(rows)
}

// ensureCorePending flips a book's park from core_needed to core_pending, unless it is
// already core_pending (idempotent - a concurrent/prior submit may have flipped it). The
// pipeline state stays contributing; SetBookStatus preserves it.
func (s *Service) ensureCorePending(ctx context.Context, book store.Book) error {
	if state.IsParkedWith(book.Status, book.ParkCode, state.ParkCorePending) {
		return nil
	}
	if err := s.deps.DB.SetBookStatus(ctx, book.ID, string(state.StatusNeedsAttention),
		s.deps.CorePendingMsg, string(state.ParkCorePending)); err != nil {
		return fmt.Errorf("contrib: park core_pending: %w", err)
	}
	return nil
}

// SetWork records a manually-supplied work slug on a book and re-admits it if it
// was parked awaiting a work (core_needed/core_pending). The slug shape is checked
// locally; existence is checked via the injected ResolveWork (a disabled metadata
// service accepts the shape alone), and a retired slug is recorded as the survivor
// it resolves to. It returns ErrInvalidSlug (bad shape) or ErrWorkNotFound
// (missing upstream) for the API to map to 400, or a wrapped transport error (502).
func (s *Service) SetWork(ctx context.Context, book store.Book, workID string) error {
	workID = strings.TrimSpace(workID)
	if !model.ValidSlug(workID) {
		return ErrInvalidSlug
	}
	live := workID
	if s.deps.ResolveWork != nil {
		var err error
		if live, err = s.deps.ResolveWork(ctx, workID); err != nil {
			if errors.Is(err, ErrWorkNotFound) {
				return ErrWorkNotFound
			}
			return fmt.Errorf("contrib: verify work: %w", err)
		}
		if live == "" {
			live = workID
		}
	}
	if _, err := AdoptLiveWork(ctx, s.deps.DB, book.ID, book.WorkID, live); err != nil {
		return fmt.Errorf("contrib: set work id: %w", err)
	}
	if awaitingWork(book) && s.deps.Readmit != nil {
		if err := s.deps.Readmit(ctx, book.ID); err != nil {
			return fmt.Errorf("contrib: re-admit book: %w", err)
		}
	}
	return nil
}

// awaitingWork reports whether a book is parked on a work it does not yet have
// (core_needed or core_pending), the two states SetWork re-admits. It shares the
// park-code predicate with the api via state.IsParkedWith.
func awaitingWork(book store.Book) bool {
	return state.IsParkedWith(book.Status, book.ParkCode, state.ParkCoreNeeded, state.ParkCorePending)
}

// publish fans a row change out as an SSE contrib.update (a nil Publish is a no-op).
func (s *Service) publish(bookID int64, kind, status, url string) {
	if s.deps.Publish == nil {
		return
	}
	s.deps.Publish(ContribUpdate{BookID: bookID, Kind: kind, Status: status, URL: url})
}

// logf logs a non-fatal poller message when a logger is configured.
func (s *Service) logf(format string, args ...any) {
	if s.deps.Log != nil {
		s.deps.Log.Warn(fmt.Sprintf(format, args...))
	}
}
