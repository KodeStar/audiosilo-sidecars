// Package scheduler drives books through the pipeline state machine over three
// concurrent lanes. One scheduler goroutine wakes on events, computes eligible
// (book, stage) pairs, and dispatches them to lane workers:
//
//   - Lane A (ASR), capacity 1: asr + retranscribing (retranscribe jumps queue).
//   - Lane B (agent books), capacity config.agent.queue_concurrency: gated by a SERIES LOCK -
//     only the lowest-position unfinished book of a series may hold an agent slot,
//     so different series parallelize but a series is authored in order.
//   - Lane C (mechanical), capacity 2: inspect/split/sanitize/qa/correct/validate/
//     contribute, running alongside ASR (CPU vs GPU).
//
// SQLite (internal/store) is the scheduling truth; the work-dir _done/<stage>.json
// sentinels are the content truth. On startup Reconcile squares the two: it
// closes stage runs interrupted by a crash and rewinds any book whose completed
// stage lost its sentinel. Business logic lives here and in internal/state;
// internal/api only calls the exported control methods.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/eta"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// Lane capacities. ASR is 1 by validated constraint; mechanical is a small fixed
// pool; the agent capacity is configurable (Config default 2).
const (
	asrCapacity  = 1
	mechCapacity = 2
)

// tickInterval is a safety re-evaluation cadence in case a wake is ever missed.
const tickInterval = 5 * time.Second

// Scheduler is the pipeline dispatcher.
type Scheduler struct {
	db       *store.DB
	hub      *events.Hub
	exec     Executor
	agentCap int
	// autoPurge reclaims a book's scratch automatically when it reaches done (and
	// startup-GCs already-done books' scratch on reconcile). Set at construction from
	// config.Contribution.AutoPurge (a New parameter, so it can never be silently
	// left disabled).
	autoPurge bool
	// workRoot is the daemon's work directory root (<data>/work). Delete removes a
	// book's scratch dir only when it lives inside this root - a guard so a
	// doctored WorkDir can never make delete rm an arbitrary path. Empty disables
	// the on-disk cleanup (tests that don't exercise it).
	workRoot string

	ctx  context.Context //nolint:containedctx // daemon-lifetime ctx for workers
	wake chan struct{}
	wg   sync.WaitGroup // in-flight workers, for a clean Stop

	mu       sync.Mutex
	inflight map[int64]*inflightBook

	// lastStats is the most recently published queue.stats, so an idle tick that
	// recomputes an identical snapshot skips the SSE frame + durable insert. Only
	// touched from the single scheduler goroutine (dispatch), so it needs no lock.
	lastStats queueStats
	haveStats bool

	// rateMu guards the in-memory per-stage unit-rate cache (seconds per unit). It is
	// seeded from the rates table at Start and updated (EWMA) by worker goroutines on
	// each successful stage run, and read by dispatch to recompute ETAs - so it needs
	// a lock even though rates persist via SetRate.
	rateMu sync.Mutex
	rates  map[string]float64

	// etaMu guards the latest published ETA snapshot: per-book remaining seconds
	// (rounded to 10s, active unparked books only) and the queue makespan. dispatch
	// writes it; the API getters read it. haveETA gates first publish.
	etaMu    sync.Mutex
	bookETA  map[int64]int64
	queueETA int64
	haveETA  bool
}

// queueStats is the published queue.stats snapshot, compared to suppress
// no-change republishes on idle ticks.
type queueStats struct {
	asr, agent, invocations, invocationCap, mech, queued int
	invocationsByBook                                    string
}

type agentInvocationRuntime interface {
	AgentInvocationRuntime() (total int, byBook map[int64]int, capacity int)
}

type agentFanoutRuntime interface {
	AgentMaxPerBook() int
}

type inflightBook struct {
	lane   state.Lane
	cancel context.CancelFunc
}

// New constructs a scheduler. agentCap < 1 is clamped to 1. workRoot is the
// daemon's <data>/work directory (Delete's on-disk cleanup is confined to it);
// pass "" to disable that cleanup. autoPurge enables automatic scratch reclamation
// (on a book reaching done, and startup-GC of already-done books) - a construction
// parameter so the feature can never be silently left off.
func New(db *store.DB, hub *events.Hub, exec Executor, agentCap int, workRoot string, autoPurge bool) *Scheduler {
	if agentCap < 1 {
		agentCap = 1
	}
	return &Scheduler{
		db:        db,
		hub:       hub,
		exec:      exec,
		agentCap:  agentCap,
		autoPurge: autoPurge,
		workRoot:  workRoot,
		wake:      make(chan struct{}, 1),
		inflight:  map[int64]*inflightBook{},
		rates:     map[string]float64{},
		bookETA:   map[int64]int64{},
	}
}

// Start reconciles crash state, then runs the dispatch loop until ctx is
// cancelled. It blocks until the loop exits and all in-flight workers finish.
func (s *Scheduler) Start(ctx context.Context) error {
	s.ctx = ctx
	s.haveStats = false // force the first pass to publish a fresh queue.stats
	// Seed the in-memory rate cache from the persisted EWMA rates so ETAs use learned
	// rates immediately (missing stages fall back to eta's in-code seeds).
	if rates, err := s.db.ListRates(ctx); err == nil {
		s.rateMu.Lock()
		s.rates = rates
		s.rateMu.Unlock()
	}
	if err := s.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	s.dispatch()
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait() // let in-flight workers observe cancellation and return
			return nil
		case <-s.wake:
			s.dispatch()
		case <-ticker.C:
			s.dispatch()
		}
	}
}

// notify wakes the dispatch loop without blocking (coalesced).
func (s *Scheduler) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Notify is the public wake used by the API after enqueuing books, so newly
// created books are dispatched immediately.
func (s *Scheduler) Notify() { s.notify() }

// --- reconcile ---

// Reconcile squares scheduling truth (DB) with content truth (sentinels) after a
// crash. It (1) closes every stage run left open (interrupted mid-execution) as
// failed, so the book re-dispatches its current stage, and (2) for each active
// book, rewinds to the earliest completed stage whose sentinel is missing (a
// purged work dir), forcing that stage to re-run.
func (s *Scheduler) Reconcile(ctx context.Context) error {
	openRuns, err := s.db.OpenStageRuns(ctx)
	if err != nil {
		return err
	}
	for _, r := range openRuns {
		if err := s.db.FinishStageRun(ctx, r.ID, false,
			json.RawMessage(`{"interrupted":true}`)); err != nil {
			return err
		}
	}

	books, err := s.db.ListBooks(ctx)
	if err != nil {
		return err
	}
	// One grouped query for every book's succeeded stages (avoids a per-book N+1
	// across the whole catalogue at startup).
	succeededByBook, err := s.db.SucceededStagesAll(ctx)
	if err != nil {
		return err
	}
	for _, b := range books {
		if state.IsTerminal(state.State(b.State)) {
			continue
		}
		if err := s.reconcileBook(ctx, b, succeededByBook[b.ID]); err != nil {
			return err
		}
	}
	if s.autoPurge {
		// Run the startup GC OFF the dispatch-gating path: purging a large done backlog
		// serially inside Reconcile would stall the first dispatch pass. Track it on the
		// WaitGroup so Stop drains it, and let it observe ctx cancellation.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.startupGC(ctx, books)
		}()
	}
	return nil
}

// startupGC reclaims the scratch of every already-done book that still accounts
// scratch on disk (a daemon that crashed before auto-purge ran, or was upgraded into
// auto-purge). It runs in a background goroutine started from Reconcile (off the
// dispatch-gating path) and only touches TERMINAL books, which no worker ever runs, so
// it needs no reservation; it observes ctx cancellation between books. Per-book
// failures are logged and skipped; one summary line reports the count reclaimed.
func (s *Scheduler) startupGC(ctx context.Context, books []store.Book) {
	purged := 0
	for _, b := range books {
		if ctx.Err() != nil {
			return
		}
		if !state.IsTerminal(state.State(b.State)) || b.ScratchBytes <= 0 {
			continue
		}
		// Reserve the book for the reclaim (mirroring PurgeScratch): a terminal book is
		// never dispatched, but a concurrent PurgeScratch/Delete could still touch it, so
		// hold the slot to serialize with them and skip a book already reserved/busy.
		if !s.reserve(b.ID) {
			continue
		}
		err := s.purgeScratchInner(ctx, b)
		s.unreserve(b.ID)
		if err != nil {
			slog.Warn("startup GC: purge failed", "book_id", b.ID, "err", err)
			continue
		}
		purged++
	}
	if purged > 0 {
		slog.Info("startup GC: reclaimed scratch for done books", "count", purged)
	}
}

// reconcileBook rewinds one active book to the earliest completed stage whose
// sentinel is missing (a purged/lost work dir), dropping the DB success of that
// stage and every later one so the counts stay honest and the stage re-runs. It is
// the per-book half of Reconcile, shared with PurgeScratch so a purge that
// invalidates a stage recovers WITHOUT waiting for a restart. Terminal books are a
// no-op. succeeded is the book's DB-succeeded stage set.
func (s *Scheduler) reconcileBook(ctx context.Context, b store.Book, succeeded map[string]bool) error {
	if state.IsTerminal(state.State(b.State)) {
		return nil
	}
	var rewind string
	haveRewind := false
	for stage := range succeeded {
		if SentinelExists(b.WorkDir, stage) {
			continue
		}
		if !haveRewind || state.Order(state.State(stage)) < state.Order(state.State(rewind)) {
			rewind, haveRewind = stage, true
		}
	}
	if !haveRewind {
		return nil
	}
	// Supersede the DB success of the rewind stage and every later completed stage so
	// their counts stay honest when the book re-advances (the rows - and their cost -
	// are preserved, only their scheduling success is retired).
	for stage := range succeeded {
		if state.Order(state.State(stage)) >= state.Order(state.State(rewind)) {
			if err := s.db.SupersedeStageSuccesses(ctx, b.ID, stage); err != nil {
				return err
			}
		}
	}
	// Preserve status/error/park_code exactly (a rewind is a position change, not a
	// status change): a parked book keeps its typed reason across a restart.
	return s.db.SetBookState(ctx, b.ID, rewind, b.Status, b.Error, b.ParkCode)
}

// --- dispatch ---

// dispatch is one scheduling pass: auto-advance waypoints, then fill each lane up
// to capacity with eligible books, then publish queue stats. It runs only in the
// scheduler goroutine, so it needs no lock beyond the inflight map.
func (s *Scheduler) dispatch() {
	ctx := s.ctx
	if ctx == nil || ctx.Err() != nil {
		return
	}
	// Timed self-resume: re-admit any book whose transient-agent park window has
	// elapsed BEFORE advancing waypoints, so a healed book is dispatched this same pass.
	s.autoReadmitDue(ctx)
	// advanceWaypoints promotes queued/ready books until none remain and returns
	// the resulting fresh list, so dispatch never re-queries.
	books, err := s.advanceWaypoints(ctx)
	if err != nil {
		return
	}
	holders := lockHolders(books)

	// Current per-lane occupancy from the inflight set.
	s.mu.Lock()
	counts := map[state.Lane]int{}
	inflightIDs := map[int64]bool{}
	for id, ib := range s.inflight {
		counts[ib.lane]++
		inflightIDs[id] = true
	}
	s.mu.Unlock()

	// Collect eligible candidates per lane.
	var asr, agent, mech []store.Book
	for _, b := range books {
		if inflightIDs[b.ID] {
			continue
		}
		st := state.State(b.State)
		if !state.CanStart(st, state.Status(b.Status), holders[b.ID]) {
			continue
		}
		switch state.LaneOf(st) {
		case state.LaneASR:
			asr = append(asr, b)
		case state.LaneAgent:
			agent = append(agent, b)
		case state.LaneMechanical:
			mech = append(mech, b)
		}
	}

	// ASR: retranscribe jumps the queue, then FIFO by id.
	sort.Slice(asr, func(i, j int) bool {
		ri := state.State(asr[i].State) == state.Retranscribing
		rj := state.State(asr[j].State) == state.Retranscribing
		if ri != rj {
			return ri
		}
		return asr[i].ID < asr[j].ID
	})
	sortByID(agent)
	sortByID(mech)

	// fillLane returns how many it dispatched, so counts ends the pass holding the
	// post-dispatch per-lane occupancy - the exact numbers queue.stats publishes,
	// with no second scan of the inflight set.
	counts[state.LaneASR] += s.fillLane(asr, state.LaneASR, asrCapacity-counts[state.LaneASR])
	counts[state.LaneAgent] += s.fillLane(agent, state.LaneAgent, s.agentCap-counts[state.LaneAgent])
	counts[state.LaneMechanical] += s.fillLane(mech, state.LaneMechanical, mechCapacity-counts[state.LaneMechanical])

	s.publishQueueStats(books, counts)
	s.publishETAs(ctx, books)
}

func sortByID(b []store.Book) {
	sort.Slice(b, func(i, j int) bool { return b[i].ID < b[j].ID })
}

// fillLane dispatches up to free candidates into a lane and returns how many it
// actually started.
func (s *Scheduler) fillLane(candidates []store.Book, lane state.Lane, free int) int {
	started := 0
	for _, b := range candidates {
		if free <= 0 {
			break
		}
		if s.startWorker(b, lane) {
			free--
			started++
		}
	}
	return started
}

// startWorker marks a book in-flight and launches its stage worker. It returns
// false if the book is already in-flight (a race with a prior pass).
func (s *Scheduler) startWorker(b store.Book, lane state.Lane) bool {
	s.mu.Lock()
	if _, busy := s.inflight[b.ID]; busy {
		s.mu.Unlock()
		return false
	}
	wctx, cancel := context.WithCancel(s.ctx)
	s.inflight[b.ID] = &inflightBook{lane: lane, cancel: cancel}
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, b.ID)
			s.mu.Unlock()
			s.notify()
		}()
		s.runStage(wctx, b)
	}()
	return true
}

// runStage executes (or, on crash-resume, skips) one stage and advances the book.
// Cancellation (pause-to-stop, cancel, or shutdown) leaves the stage re-runnable:
// it closes the run failed but does not change book state.
func (s *Scheduler) runStage(ctx context.Context, b store.Book) {
	stage := state.State(b.State)
	stageName := string(stage)

	// Crash-resume fast path: the sentinel already exists (a crash happened after
	// the executor wrote it but before the advance). Recover the branch decision
	// and advance WITHOUT opening a new stage_run - re-recording the run would
	// double-count a stage that genuinely completed. This check runs BEFORE
	// StartStageRun so no phantom run row is ever created for a skipped stage.
	if SentinelExists(b.WorkDir, stageName) {
		if sn, rerr := ReadSentinel(b.WorkDir, stageName); rerr == nil {
			s.advance(ctx, b, stage, sn.Result)
			return
		}
		// An unreadable sentinel falls through to a fresh execution below.
	}

	n, err := s.db.CountStageRuns(ctx, b.ID, stageName)
	if err != nil {
		return
	}
	runID, err := s.db.StartStageRun(ctx, b.ID, stageName, n+1)
	if err != nil {
		return
	}

	result, err := s.execute(ctx, b, stage)
	if errors.Is(err, context.Canceled) {
		// Paused/cancelled/shutting down: the worker ctx is already cancelled, so
		// close the run on a fresh context (mirroring setStatus) - otherwise the
		// row stays open forever and reconcile has to close it on the next boot.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.db.FinishStageRun(closeCtx, runID, false, json.RawMessage(`{"cancelled":true}`))
		return
	}
	if err != nil {
		_ = s.db.FinishStageRun(ctx, runID, false, metricsErr(err))
		// A ParkError is a deliberate "a human must act" stop (e.g. an unimplemented
		// stage), so park the book needs_attention rather than flag a hard failure.
		var pe *ParkError
		if errors.As(err, &pe) {
			// A ParkError may carry a RetryAfter (a transient agent condition), which the
			// dispatch loop's auto-readmit acts on once due; a plain park passes the zero time.
			s.setStatus(b.ID, state.StatusNeedsAttention, pe.Error(), pe.Code, pe.RetryAfter)
		} else {
			s.setStatus(b.ID, state.StatusFailed, err.Error(), "", time.Time{})
		}
		return
	}
	// A stage that returns success MUST have written its sentinel (the content-truth
	// marker crash-resume relies on) as its final durable action. If it did not,
	// advancing would spin silently forever: the next reconcile finds the sentinel
	// missing, rewinds to re-run the stage, and the stage again "succeeds" with no
	// sentinel. Turn that stage-implementation bug into a loud, terminal failure.
	if !SentinelExists(b.WorkDir, stageName) {
		serr := fmt.Errorf("stage %q returned success without writing its sentinel - bug in the stage implementation", stageName)
		_ = s.db.FinishStageRun(ctx, runID, false, metricsErr(serr))
		s.setStatus(b.ID, state.StatusFailed, serr.Error(), "", time.Time{})
		return
	}
	if ferr := s.db.FinishStageRun(ctx, runID, true, result.Metrics); ferr != nil {
		return
	}
	// Fold this run's observed unit-rate into the EWMA cache from the stage's own
	// RateSample - the units it actually processed and the productive seconds it spent
	// (setup, tool/model downloads and rate-limit backoff excluded), so a resumed run
	// measures only what it did. nil sample = no observation.
	s.recordRate(ctx, stageName, result.RateSample)
	s.advance(ctx, b, stage, result)
}

// execute runs the injected executor with a progress reporter that persists and
// publishes stage.progress. Progress is display-only now (the learned rate comes from
// the stage's StageResult.RateSample), so the reporter no longer tracks a span.
func (s *Scheduler) execute(ctx context.Context, b store.Book, stage state.State) (StageResult, error) {
	r := StageReport{
		Progress: func(done, total int) {
			_ = s.db.SetProgress(ctx, b.ID, string(stage), done, total)
			_ = s.db.TouchOpenStageRun(ctx, b.ID, string(stage), true)
			_ = s.hub.PublishBook("stage.progress", b.ID, map[string]any{
				"book_id": b.ID, "stage": string(stage), "done": done, "total": total,
			})
		},
		// Note emits a human-readable line into the book's durable log (a stage.note
		// event on the same PublishBook -> persister -> store.events path as
		// stage.progress, so GET /books/{id}/events returns it).
		Note: func(msg string) {
			_ = s.hub.PublishBook("stage.note", b.ID, map[string]any{
				"book_id": b.ID, "stage": string(stage), "msg": msg,
			})
		},
	}
	return s.exec.Execute(ctx, b, stage, r)
}

// recordRate folds one successful stage run's RateSample into the per-stage EWMA rate
// cache and persists it. It fires ONLY when the stage reported a sample with positive
// units and seconds; a nil or non-positive sample is a no-op. Safe for concurrent
// worker goroutines (guarded by rateMu).
func (s *Scheduler) recordRate(ctx context.Context, stage string, sample *RateSample) {
	if sample == nil || sample.Units <= 0 || sample.Seconds <= 0 {
		return
	}
	s.rateMu.Lock()
	newRate, ok := eta.Observe(stage, sample.Seconds, sample.Units, s.rates)
	if ok {
		s.rates[stage] = newRate
	}
	s.rateMu.Unlock()
	if ok {
		_ = s.db.SetRate(ctx, stage, newRate)
	}
}

// advance computes the next state from the completed stage's result and applies
// it, publishing book.state. The audit fix-loop cap parks the book instead.
func (s *Scheduler) advance(ctx context.Context, b store.Book, stage state.State, result StageResult) {
	out := state.Outcome{
		MarkersContiguous:  result.MarkersContiguous,
		QAClean:            result.QAClean,
		RetranscribeNeeded: result.RetranscribeNeeded,
		AuditPassed:        result.AuditPassed,
	}
	if stage == state.Auditing {
		fixes, err := s.db.CountStageSuccesses(ctx, b.ID, string(state.Fixing))
		if err != nil {
			return
		}
		out.FixAttempts = fixes
	}

	next, status, err := state.NextState(stage, out)
	if err != nil {
		s.setStatus(b.ID, state.StatusFailed, err.Error(), "", time.Time{})
		return
	}
	if status == state.StatusNeedsAttention {
		// Park: keep the state, flag needs_attention (audit unresolved after the fix
		// loop is exhausted), carrying the typed fix-loop-exhausted park code. The
		// auditing stage attaches a fix-count trajectory (ParkMessage) so the reason
		// explains WHY it did not converge; fall back to the generic message when absent
		// (a legacy/pre-change sentinel or a stage that set none).
		msg := "audit failed after maximum fix attempts"
		if result.ParkMessage != "" {
			msg = result.ParkMessage
		}
		s.setStatus(b.ID, state.StatusNeedsAttention, msg, state.ParkFixLoopExhausted, time.Time{})
		return
	}

	// A transition INTO next always means a fresh execution of next, so drop any
	// stale sentinel for it from a prior loop-back (retranscribing->qa_sweep,
	// fixing->validating, or a re-entered qa_adjudicating). Otherwise runStage would
	// skip next as "already done" and replay a frozen outcome. Crash-resume never
	// routes through advance (it re-dispatches at the current state, where skipping
	// IS correct), so this only ever clears a genuine re-entry.
	_ = os.Remove(SentinelPath(b.WorkDir, string(next)))

	// Advance the pipeline state ONLY: status and error belong to the control path
	// (pause/cancel/fail). Writing them here would clobber a pause/cancel that
	// landed while this stage was finishing, and would wipe any error. The book was
	// dispatched under StatusNone, so a normal advance publishes StatusNone.
	if err := s.db.SetBookPipelineState(ctx, b.ID, string(next)); err != nil {
		return
	}
	s.publishState(b.ID, string(next), "", "", "", "")

	// Auto-purge: a book that just reached done no longer needs its scratch. The worker
	// already holds this book's in-flight slot, so reclaim WITHOUT reserving (that would
	// see itself busy). A failure is logged, never fails the stage.
	if next == state.Done && s.autoPurge {
		s.autoPurgeDone(ctx, b.ID)
	}
}

// autoPurgeDone reclaims a just-completed book's scratch. It re-reads the book (its
// state is now done, so the shared purge helper skips the reconcile) and calls the
// no-reserve purge body. A failure is logged, never propagated.
func (s *Scheduler) autoPurgeDone(ctx context.Context, id int64) {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return
	}
	if !purgeAllowed(b) {
		return
	}
	if err := s.purgeScratchInner(ctx, b); err != nil {
		slog.Warn("auto-purge after done failed", "book_id", id, "err", err)
	}
}

// advanceWaypoints promotes queued/ready books (no lane, no executor) to their
// next state until none remain, so the machine never stalls on a waypoint. It
// loops until a pass advances nothing and returns that final, fresh book list so
// dispatch can act on it without re-querying.
func (s *Scheduler) advanceWaypoints(ctx context.Context) ([]store.Book, error) {
	// One initial query; subsequent passes advance the in-memory slice so a chain
	// of waypoints (queued -> inspecting, ready -> contributing) needs no re-read.
	books, err := s.db.ListBooks(ctx)
	if err != nil {
		return nil, err
	}
	for {
		advanced := false
		for i := range books {
			b := books[i]
			st := state.State(b.State)
			if b.Status != "" || !state.IsWaypoint(st) {
				continue
			}
			next, _, err := state.NextState(st, state.Outcome{})
			if err != nil {
				continue
			}
			if err := s.db.SetBookState(ctx, b.ID, string(next), "", "", ""); err != nil {
				continue
			}
			books[i].State = string(next) // mirror the persisted advance in memory
			s.publishState(b.ID, string(next), "", "", "", "")
			advanced = true
		}
		if !advanced {
			return books, nil
		}
	}
}

// autoReadmitCodes are the park reasons the timed self-resume acts on: the transient
// agent conditions a wait heals (the CLI symlink blip recovers, or the rate-limit
// window elapses). Every other park (a not-confident marker verdict, a spelling gate
// failure, a budget stop, ...) needs a human and is never auto-readmitted.
var autoReadmitCodes = []state.ParkCode{state.ParkAgentUnavailable, state.ParkAgentRateLimited}

// autoReadmitDue re-admits every book whose scheduled retry_at has elapsed and whose
// park code is one the timed self-resume owns (autoReadmitCodes). It re-admits through
// the SAME readmit path a manual Retry uses (clearing status/error/park_code/retry_at
// and forcing the current stage to re-run) and drops a durable stage.note so the log
// shows the automatic re-admit. A book parked before migration 0008 has retry_at=” and
// is never returned by the query, so it only ever re-admits via a human Retry. Errors
// are logged and skipped - one stuck book must not stall the whole dispatch pass.
func (s *Scheduler) autoReadmitDue(ctx context.Context) {
	now := time.Now().UTC().Format(time.RFC3339)
	due, err := s.db.ListBooksDueForRetry(ctx, now)
	if err != nil {
		slog.Warn("auto-readmit: query due books failed", "err", err)
		return
	}
	for _, b := range due {
		if !state.IsParkedWith(b.Status, b.ParkCode, autoReadmitCodes...) {
			continue
		}
		// Reserve so a concurrent PurgeScratch/Delete/manual-Retry does not race the
		// re-admit; a book already in-flight (it should not be, while parked) is skipped.
		if !s.reserve(b.ID) {
			continue
		}
		err := s.readmit(ctx, b)
		s.unreserve(b.ID)
		if err != nil {
			slog.Warn("auto-readmit: readmit failed", "book_id", b.ID, "err", err)
			continue
		}
		_ = s.hub.PublishBook("stage.note", b.ID, map[string]any{
			"book_id": b.ID, "stage": b.State,
			"msg": "auto-retry: agent availability window elapsed",
		})
	}
}

// --- series lock ---

// lockHolders returns the set of book ids permitted to run an agent stage: the
// lowest-position unfinished book in each series, plus every seriesless book
// (each parallelizes freely). A book that has reached ready (or beyond) no longer
// holds its series.
//
// "Unfinished" is purely positional (HoldsSeriesLock tests state order, not
// status), so a pre-Ready predecessor that is PARKED (needs_attention) OR FAILED/
// CANCELLED (status=failed) still holds its series lock and blocks its successors'
// agent work until the user retries or deletes it. This is deliberately
// conservative: series carryover (the "story so far" recaps) wants earlier books
// authored first, so a stuck predecessor should hold the line rather than let a
// later book jump ahead. The plan flags this as a behaviour to revisit once real
// runs show how often a predecessor gets stuck.
func lockHolders(books []store.Book) map[int64]bool {
	holders := map[int64]bool{}
	best := map[string]store.Book{}
	for _, b := range books {
		if !state.HoldsSeriesLock(state.State(b.State)) {
			continue // finished for lock purposes (ready or beyond)
		}
		if strings.TrimSpace(b.Series) == "" {
			holders[b.ID] = true
			continue
		}
		cur, ok := best[b.Series]
		if !ok || seriesLess(b, cur) {
			best[b.Series] = b
		}
	}
	for _, b := range best {
		holders[b.ID] = true
	}
	return holders
}

// seriesLess orders two books of the same series by numeric position, then id. It
// parses positions with state.ParseSeriesPos (the pure leaf the scheduler, the ETA
// queue simulation, and the pipeline's series-carryover discovery all share).
func seriesLess(a, b store.Book) bool {
	pa, pb := state.ParseSeriesPos(a.SeriesPos), state.ParseSeriesPos(b.SeriesPos)
	if pa != pb {
		return pa < pb
	}
	return a.ID < b.ID
}

// --- events ---

// publishState emits a book.state SSE frame. retryAt is the scheduled auto-readmit
// instant (RFC3339 UTC) when the write set one, else "" - it rides the patch so a client
// can flip a parked book's hint to "retries automatically" (and clear it on re-admit)
// without a separate GET. Every caller but setStatus passes "" (no scheduled retry).
func (s *Scheduler) publishState(bookID int64, st, status, errMsg, parkCode, retryAt string) {
	_ = s.hub.PublishBook("book.state", bookID, map[string]any{
		"book_id":   bookID,
		"state":     st,
		"lane":      string(state.LaneOf(state.State(st))),
		"status":    status,
		"error":     errMsg,
		"park_code": parkCode,
		"retry_at":  retryAt,
	})
}

// publishQueueStats publishes queue.stats from the counts dispatch already
// computed. It publishes only when the snapshot differs from the last one it
// published, so an idle 5s tick that recomputes an identical snapshot does not
// emit an SSE frame, persist a durable row, or re-render every client. Start
// resets the dedup so the first pass always publishes.
func (s *Scheduler) publishQueueStats(books []store.Book, counts map[state.Lane]int) {
	queued := 0
	for _, b := range books {
		st := state.State(b.State)
		if b.Status == "" && !state.IsTerminal(st) {
			queued++
		}
	}
	invocations, invocationCap := 0, s.agentCap
	perBook := map[int64]int{}
	if runtime, ok := s.exec.(agentInvocationRuntime); ok {
		invocations, perBook, invocationCap = runtime.AgentInvocationRuntime()
	}
	perBookJSON, _ := json.Marshal(perBook) // int-key map is emitted in stable key order
	next := queueStats{
		asr:               counts[state.LaneASR],
		agent:             counts[state.LaneAgent],
		invocations:       invocations,
		invocationCap:     invocationCap,
		invocationsByBook: string(perBookJSON),
		mech:              counts[state.LaneMechanical],
		queued:            queued,
	}
	if s.haveStats && next == s.lastStats {
		return
	}
	s.lastStats, s.haveStats = next, true
	_ = s.hub.Publish("queue.stats", map[string]any{
		"asr_active":                next.asr,
		"agent_active":              next.agent,
		"agent_books_active":        next.agent,
		"agent_book_capacity":       s.agentCap,
		"agent_invocations_active":  next.invocations,
		"agent_invocation_capacity": next.invocationCap,
		"agent_invocations_by_book": perBook,
		"mechanical_active":         next.mech,
		"queued":                    next.queued,
	})
}

// setStatus writes an orthogonal status flag (plus the free-text error and the
// typed park code) and publishes book.state, so a client can surface both the
// reason string and the machine-readable park class. parkCode is the typed reason
// for a needs_attention park; callers pass "" for a plain failure or a clear, so
// the code stays in sync with the status (set on a park, empty otherwise). retryAt,
// when non-zero, schedules an automatic re-admit (persisted to books.retry_at in UTC
// RFC3339): the transient-agent parks pass a due time so the dispatch loop heals the
// book without a human; every other caller passes the zero time (no scheduled retry).
func (s *Scheduler) setStatus(bookID int64, status state.Status, errMsg string, parkCode state.ParkCode, retryAt time.Time) {
	ctx := context.Background()
	b, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return
	}
	retryStr := ""
	if !retryAt.IsZero() {
		retryStr = retryAt.UTC().Format(time.RFC3339)
	}
	if err := s.db.SetBookStatusRetry(ctx, bookID, string(status), errMsg, string(parkCode), retryStr); err != nil {
		return
	}
	s.publishState(bookID, b.State, string(status), errMsg, string(parkCode), retryStr)
}

func metricsErr(err error) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return b
}
