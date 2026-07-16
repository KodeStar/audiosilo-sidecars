package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/kodestar/audiosilo-sidecars/internal/scratch"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// ErrInvalidOp is returned when a control action does not apply to a book's
// current status (e.g. resuming a book that is not paused).
var ErrInvalidOp = errors.New("operation not valid for book's current status")

// ErrBusy is returned by Delete when a book is actively running a stage.
var ErrBusy = errors.New("book is running a stage")

// cancelInflight cancels a book's running stage if any (interrupting its
// executor); the worker then leaves the stage re-runnable.
func (s *Scheduler) cancelInflight(id int64) {
	s.mu.Lock()
	ib, ok := s.inflight[id]
	s.mu.Unlock()
	if ok {
		ib.cancel()
	}
}

// Pause parks a running or queued book: it stops being dispatched. A stage
// already executing is allowed to finish (its result is kept), after which the
// book waits in its next state. Pausing an already-paused book is a no-op.
func (s *Scheduler) Pause(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	switch state.Status(b.Status) {
	case state.StatusPaused:
		return nil
	case state.StatusNone:
		if err := s.db.SetBookStatus(ctx, id, string(state.StatusPaused), b.Error); err != nil {
			return err
		}
		s.publishState(id, b.State, string(state.StatusPaused), b.Error)
		return nil
	default:
		return ErrInvalidOp
	}
}

// Resume clears a pause and re-admits the book to scheduling.
func (s *Scheduler) Resume(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	if state.Status(b.Status) != state.StatusPaused {
		return ErrInvalidOp
	}
	if err := s.db.SetBookStatus(ctx, id, string(state.StatusNone), ""); err != nil {
		return err
	}
	s.publishState(id, b.State, "", "")
	s.notify()
	return nil
}

// Retry re-admits a failed or needs_attention book by clearing its status and
// forcing the current stage to re-run: the stage's sentinel and its recorded
// success are dropped so the executor runs afresh (a failed stage has no sentinel
// anyway; a parked audit does, and must be re-executed to make progress).
func (s *Scheduler) Retry(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	switch state.Status(b.Status) {
	case state.StatusFailed, state.StatusNeedsAttention:
		stage := b.State
		_ = os.Remove(SentinelPath(b.WorkDir, stage)) // best-effort; force a clean re-run
		if err := s.db.DeleteStageSuccess(ctx, id, stage); err != nil {
			return err
		}
		if err := s.db.SetBookStatus(ctx, id, string(state.StatusNone), ""); err != nil {
			return err
		}
		s.publishState(id, b.State, "", "")
		s.notify()
		return nil
	default:
		return ErrInvalidOp
	}
}

// Cancel stops a book: it interrupts any running stage and marks the book failed
// with a "cancelled" reason (the status enum has no dedicated cancelled state).
// A cancelled book can later be retried or deleted.
func (s *Scheduler) Cancel(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	if state.IsTerminal(state.State(b.State)) {
		return ErrInvalidOp
	}
	if err := s.db.SetBookStatus(ctx, id, string(state.StatusFailed), "cancelled by user"); err != nil {
		return err
	}
	s.cancelInflight(id)
	s.publishState(id, b.State, string(state.StatusFailed), "cancelled by user")
	return nil
}

// Delete removes a book, its durable state, and its on-disk work dir. It refuses
// while the book is actively running a stage (cancel or pause first), so a worker
// never writes to a deleted book. The in-flight check and the DB delete happen
// under one lock so a worker can't start in the window between them; the books FK
// (ON DELETE CASCADE) still leaves a narrow residual race if a worker is mid-write
// - the user can simply re-delete.
func (s *Scheduler) Delete(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if _, busy := s.inflight[id]; busy {
		s.mu.Unlock()
		return ErrBusy
	}
	err = s.db.DeleteBook(ctx, id)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.removeWorkDir(b.WorkDir)
	return nil
}

// purgeInvalidatedStages are the stage sentinels a scratch purge must drop so the
// book re-runs those stages instead of trusting a stale "done" marker. Their
// durable output (the chapter FLACs) IS the scratch scratch.Purge reclaims, so
// after a purge the content-truth sentinel would otherwise let runStage's crash-
// resume fast-path skip the stage and hand a later stage (ASR, M3) an empty
// chapters/. Coupled to scratch.Purge's reclaim set - it grows as later milestones
// reclaim more scratch (e.g. ASR working files in M3).
var purgeInvalidatedStages = []state.State{state.Splitting}

// PurgeScratch reclaims a book's split chapters/ (the M2 heavy scratch) while
// keeping its durables (probe.json/manifest.json/transcripts). It is a manual,
// user-initiated reclaim: allowed only when the book is done, paused, or
// failed/cancelled - never mid-run (a running book still needs its chapters).
//
// It reserves the book id for the duration (a pseudo-inflight entry) so a
// concurrent Resume/Retry-triggered dispatch cannot start a stage that races the
// chapter removal, and a concurrent Delete sees it busy (ErrBusy) - the same
// window Delete already guards. The removal is confined to the work root by
// scratch.Purge.
func (s *Scheduler) PurgeScratch(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	if !purgeAllowed(b) {
		return ErrInvalidOp
	}
	// Reserve under the same lock dispatch/Delete use: fail if already in-flight,
	// otherwise hold the slot so no worker starts for this id until we release it.
	if !s.reserve(id) {
		return ErrBusy
	}
	defer s.unreserve(id)

	if err := scratch.Purge(s.workRoot, b.WorkDir); err != nil {
		return err
	}
	// The reclaimed artifacts include a completed stage's output, so drop that
	// stage's sentinel (and its recorded success) - a later Retry/reconcile must
	// re-run it rather than skip it and feed the next stage an empty chapters/.
	for _, st := range purgeInvalidatedStages {
		_ = os.Remove(SentinelPath(b.WorkDir, string(st)))
	}
	// Re-account what remains (the durables) in one walk so scratch_bytes reflects
	// the reclaim without a read-side walk. If the walk itself fails, the pre-purge
	// value is now definitely wrong (we just deleted the chapters), so record 0
	// rather than leave a stale over-count.
	if n, derr := scratch.DirSize(b.WorkDir); derr == nil {
		_ = s.db.UpdateScratchBytes(ctx, id, n)
	} else {
		slog.Warn("purge: re-accounting the work dir failed; recording 0 scratch",
			"book_id", id, "work_dir", b.WorkDir, "err", derr)
		_ = s.db.UpdateScratchBytes(ctx, id, 0)
	}
	// Reconcile the purged book WITHOUT waiting for a restart: dropping the split
	// sentinel above only re-runs the book if it is still AT splitting. A book past
	// splitting (e.g. failed at asr) would otherwise retry into an empty chapters/
	// and fail. reconcileBook rewinds it to the earliest completed stage whose
	// sentinel we just invalidated, so a following Retry re-splits. Terminal (done)
	// books are left untouched. It runs while the id is still reserved.
	if !state.IsTerminal(state.State(b.State)) {
		succeeded, serr := s.db.SucceededStages(ctx, id)
		if serr != nil {
			return serr
		}
		if rerr := s.reconcileBook(ctx, b, succeeded); rerr != nil {
			return rerr
		}
	}
	return nil
}

// reserve inserts a pseudo-inflight entry for id (lane-less, cancel is a no-op) so
// dispatch/startWorker skip it and Delete treats it as busy. It returns false if
// the id is already in-flight (a real worker or another reservation). Callers pair
// it with unreserve.
func (s *Scheduler) reserve(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.inflight[id]; busy {
		return false
	}
	s.inflight[id] = &inflightBook{lane: state.LaneNone, cancel: func() {}}
	return true
}

// unreserve drops a reservation and wakes the dispatch loop, so a book made
// dispatchable during the reservation (e.g. a Resume that landed mid-purge) is
// picked up now that the slot is free.
func (s *Scheduler) unreserve(id int64) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
	s.notify()
}

// purgeAllowed reports whether a book is in a state where reclaiming its chapters
// is safe: terminal (done), or parked paused/failed (cancel marks a book failed).
// A running book (status none, non-terminal) still needs its chapters.
func purgeAllowed(b store.Book) bool {
	if state.IsTerminal(state.State(b.State)) {
		return true
	}
	switch state.Status(b.Status) {
	case state.StatusPaused, state.StatusFailed:
		return true
	default:
		return false
	}
}

// removeWorkDir deletes a removed book's scratch dir, but ONLY when it resolves to
// a path inside the daemon's work root - a guard so a doctored or legacy WorkDir
// can never make delete rm an arbitrary location. It shares scratch.Confined with
// the purge path, so both destructive operations enforce the one guard. A missing
// dir is fine.
func (s *Scheduler) removeWorkDir(workDir string) {
	if wd, ok := scratch.Confined(s.workRoot, workDir); ok {
		_ = os.RemoveAll(wd)
	}
}
