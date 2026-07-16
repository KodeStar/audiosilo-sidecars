package scheduler

import (
	"context"
	"math/rand"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// ProgressFunc reports within-stage progress (chapter i/N, chunk i/N) so the
// scheduler can persist it and publish a stage.progress event.
type ProgressFunc func(done, total int)

// Executor runs one pipeline stage for a book. The real ASR/agent/mechanical
// executors land in M2+; M1 ships StubExecutor. An executor MUST, on success,
// call WriteSentinel(book.WorkDir, string(stage), result) as its final durable
// action - that is the content-truth marker the scheduler's crash-resume relies
// on. It returns an error (and writes no sentinel) on failure or cancellation.
type Executor interface {
	Execute(ctx context.Context, book store.Book, stage state.State, report ProgressFunc) (StageResult, error)
}

// ParkError is an executor error that asks the scheduler to park the book
// needs_attention (a human must act) instead of marking it failed. It suits a
// known, non-transient stop - an unimplemented stage, or an input the automatic
// pipeline cannot yet handle - where a blind Retry would just fail again. runStage
// maps it to StatusNeedsAttention (carrying Reason), so the book waits in the
// Running tab flagged for attention rather than as an error.
type ParkError struct{ Reason string }

func (e *ParkError) Error() string { return e.Reason }

// Park builds a ParkError with the given human-facing reason.
func Park(reason string) error { return &ParkError{Reason: reason} }

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
	rng    *rand.Rand
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
		rng:      rand.New(rand.NewSource(1)), //nolint:gosec // not security-sensitive
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
func (e *StubExecutor) Execute(ctx context.Context, book store.Book, stage state.State, report ProgressFunc) (StageResult, error) {
	span := e.MaxDelay - e.MinDelay
	total := e.MinDelay
	if span > 0 {
		total += time.Duration(e.rng.Int63n(int64(span)))
	}
	const ticks = 2
	if report != nil {
		report(0, ticks)
	}
	for i := 1; i <= ticks; i++ {
		select {
		case <-ctx.Done():
			return StageResult{}, ctx.Err()
		case <-time.After(total / ticks):
		}
		if report != nil {
			report(i, ticks)
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
