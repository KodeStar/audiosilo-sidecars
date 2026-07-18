package scheduler

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// ProgressFunc reports within-stage progress (chapter i/N, chunk i/N) so the
// scheduler can persist it and publish a stage.progress event.
//
// RESUME-BASELINE REPORTING (display convention): the FIRST report of a run is the
// already-complete unit count - the resume baseline, e.g. 0 on a fresh run or the
// number of chapters already transcribed on a resumed one - so a client's progress
// bar starts at the resume point instead of jumping back to 0. Subsequent reports
// tick as units complete. This is purely display semantics: the learned per-unit rate
// no longer derives from these values (the stage reports its own StageResult.RateSample
// with the units it actually processed and the productive seconds it spent), so a stage
// that ticks through skipped units affects only the bar, never the rate.
type ProgressFunc func(done, total int)

// StageReport is the reporting channel every executor stage receives. It carries two
// independent signals:
//
//   - Progress reports numeric within-stage progress (the existing stage.progress
//     behavior; see ProgressFunc for the resume-baseline convention).
//   - Note emits a human-readable line into the book's durable log (a stage.note
//     event): a descriptive stage-entry line ("re-transcribing 3 chapters: 5, 12, 18")
//     and the agent liveness heartbeat ("still running (6m elapsed)"). It is a real
//     signal, not decoration - the heartbeat fires only while an agent subprocess is
//     genuinely running.
//
// Either field may be nil (a stage guards each call); the zero StageReport is a valid
// no-op reporter for tests.
type StageReport struct {
	Progress ProgressFunc
	Note     func(msg string)
}

// RateSample is a stage's own report of how much work it did in ONE run, used to update
// the per-stage EWMA seconds-per-unit rate. Units is how many units the stage actually
// processed this run (chapters split, chunks completed, or 1 for a whole-book stage);
// Seconds is the productive time it spent on them, EXCLUDING setup, tool/model
// downloads, and rate-limit backoff sleeps. A nil *RateSample (or non-positive Units/
// Seconds) means "no rate observation" and the scheduler skips the update.
type RateSample struct {
	Units   int
	Seconds float64
}

// Executor runs one pipeline stage for a book. The real ASR/agent/mechanical
// executors land in M2+; M1 ships StubExecutor. An executor MUST, on success,
// call WriteSentinel(book.WorkDir, string(stage), result) as its final durable
// action - that is the content-truth marker the scheduler's crash-resume relies
// on. It returns an error (and writes no sentinel) on failure or cancellation.
type Executor interface {
	Execute(ctx context.Context, book store.Book, stage state.State, r StageReport) (StageResult, error)
}

// ParkError is an executor error that asks the scheduler to park the book
// needs_attention (a human must act) instead of marking it failed. It suits a
// known, non-transient stop - an unimplemented stage, or an input the automatic
// pipeline cannot yet handle - where a blind Retry would just fail again. runStage
// maps it to StatusNeedsAttention (carrying Reason and the typed Code), so the book
// waits in the Running tab flagged for attention rather than as an error.
//
// Code is the machine-readable park reason persisted to books.park_code and
// published on book.state (empty when the park has no typed code); Reason is the
// human-facing message.
//
// RetryAfter, when non-zero, schedules an AUTOMATIC re-admit: the scheduler persists it
// to books.retry_at and a later dispatch pass re-admits the book once the instant
// passes (used for the transient agent-unavailable/rate-limited parks so an overnight
// batch heals itself). A zero RetryAfter is a human-only park (Retry re-admits it).
type ParkError struct {
	Reason     string
	Code       state.ParkCode
	RetryAfter time.Time
}

func (e *ParkError) Error() string { return e.Reason }

// Park builds a ParkError with the given human-facing reason and no typed code.
func Park(reason string) error { return &ParkError{Reason: reason} }

// ParkWithCode builds a ParkError carrying both the human-facing reason and a
// machine-readable ParkCode. The scheduler persists the code to books.park_code and
// emits it on the book.state event so the UI can render a per-class affordance.
func ParkWithCode(code state.ParkCode, reason string) error {
	return &ParkError{Reason: reason, Code: code}
}

// ParkWithCodeAfter is ParkWithCode plus a RetryAfter instant: the scheduler stamps
// books.retry_at and auto-readmits the book once it passes. Used for the transient
// agent parks (unavailable/rate-limited) so a book self-resumes when the CLI returns or
// the rate-limit window elapses, without waiting for a human Retry.
func ParkWithCodeAfter(code state.ParkCode, reason string, retryAfter time.Time) error {
	return &ParkError{Reason: reason, Code: code, RetryAfter: retryAfter}
}

// StubExecutor is the M1 placeholder executor: it sleeps a short, bounded time
// (so the whole state machine runs end to end and lanes visibly occupy), reports
// a couple of progress ticks, then writes the stage sentinel. Outcomes are
// deterministic. By default it takes the happy path (skip the conditional
// stages, pass the audit); a test can override per-stage decisions via Decide.
type StubExecutor struct {
	MinDelay time.Duration
	MaxDelay time.Duration
	// Decide, when set, returns the branch decision for a (book, stage). When nil
	// the happy-path defaults apply.
	Decide func(book store.Book, stage state.State) StageResult
}

// NewStubExecutor returns a stub with the given per-stage delay bounds. Zero
// bounds default to 50-200ms.
func NewStubExecutor(minDelay, maxDelay time.Duration) *StubExecutor {
	if minDelay <= 0 {
		minDelay = 50 * time.Millisecond
	}
	if maxDelay < minDelay {
		maxDelay = 200 * time.Millisecond
	}
	return &StubExecutor{
		MinDelay: minDelay,
		MaxDelay: maxDelay,
	}
}

// happyPath is the default branch decision: skip the optional stages and pass the
// audit, so a book runs straight to done.
func happyPath() StageResult {
	return StageResult{MarkersContiguous: true, QAClean: true, AuditPassed: true}
}

// Execute sleeps, reports progress, and writes the sentinel. It respects ctx
// cancellation (returning ctx.Err() without writing a sentinel) so a paused/
// cancelled/shutting-down daemon leaves the stage re-runnable.
func (e *StubExecutor) Execute(ctx context.Context, book store.Book, stage state.State, r StageReport) (StageResult, error) {
	span := e.MaxDelay - e.MinDelay
	total := e.MinDelay
	if span > 0 {
		// rand/v2's top-level functions are safe for concurrent use - lane
		// workers share this executor, so a per-struct rand.Rand would race.
		total += time.Duration(rand.Int64N(int64(span))) //nolint:gosec // stub delay jitter, not security-sensitive
	}
	const ticks = 2
	if r.Progress != nil {
		r.Progress(0, ticks)
	}
	for i := 1; i <= ticks; i++ {
		select {
		case <-ctx.Done():
			return StageResult{}, ctx.Err()
		case <-time.After(total / ticks):
		}
		if r.Progress != nil {
			r.Progress(i, ticks)
		}
	}

	result := happyPath()
	if e.Decide != nil {
		result = e.Decide(book, stage)
	}
	if err := WriteSentinel(book.WorkDir, string(stage), result); err != nil {
		return StageResult{}, err
	}
	return result, nil
}
