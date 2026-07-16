// Package scheduler drives books through the pipeline state machine over three
// concurrent lanes. One scheduler goroutine wakes on events, computes eligible
// (book, stage) pairs, and dispatches them to lane workers:
//
//   - Lane A (ASR), capacity 1: asr + retranscribing (retranscribe jumps queue).
//   - Lane B (agent), capacity config.agent.concurrency: gated by a SERIES LOCK -
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
}

// queueStats is the published queue.stats snapshot, compared to suppress
// no-change republishes on idle ticks.
type queueStats struct {
	asr, agent, mech, queued int
}

type inflightBook struct {
	lane   state.Lane
	cancel context.CancelFunc
}

// New constructs a scheduler. agentCap < 1 is clamped to 1.
func New(db *store.DB, hub *events.Hub, exec Executor, agentCap int) *Scheduler {
	if agentCap < 1 {
		agentCap = 1
	}
	return &Scheduler{
		db:       db,
		hub:      hub,
		exec:     exec,
		agentCap: agentCap,
		wake:     make(chan struct{}, 1),
		inflight: map[int64]*inflightBook{},
	}
}

// Start reconciles crash state, then runs the dispatch loop until ctx is
// cancelled. It blocks until the loop exits and all in-flight workers finish.
func (s *Scheduler) Start(ctx context.Context) error {
	s.ctx = ctx
	s.haveStats = false // force the first pass to publish a fresh queue.stats
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
		succeeded := succeededByBook[b.ID]
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
			continue
		}
		// Drop the DB success of the rewind stage and every later completed stage
		// so their counts stay honest when the book re-advances.
		for stage := range succeeded {
			if state.Order(state.State(stage)) >= state.Order(state.State(rewind)) {
				if err := s.db.DeleteStageSuccess(ctx, b.ID, stage); err != nil {
					return err
				}
			}
		}
		if err := s.db.SetBookState(ctx, b.ID, rewind, b.Status, b.Error); err != nil {
			return err
		}
	}
	return nil
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

// runStage executes (or skips, if the sentinel already exists) one stage and
// advances the book. Cancellation (pause-to-stop, cancel, or shutdown) leaves the
// stage re-runnable: it closes the run failed but does not change book state.
func (s *Scheduler) runStage(ctx context.Context, b store.Book) {
	stage := state.State(b.State)
	stageName := string(stage)

	n, err := s.db.CountStageRuns(ctx, b.ID, stageName)
	if err != nil {
		return
	}
	runID, err := s.db.StartStageRun(ctx, b.ID, stageName, n+1)
	if err != nil {
		return
	}

	var result StageResult
	if SentinelExists(b.WorkDir, stageName) {
		// Crash after the sentinel was written but before the advance: recover the
		// branch decision and skip re-execution.
		if sn, rerr := ReadSentinel(b.WorkDir, stageName); rerr == nil {
			result = sn.Result
		} else {
			result, err = s.execute(ctx, b, stage)
		}
	} else {
		result, err = s.execute(ctx, b, stage)
	}
	if errors.Is(err, context.Canceled) {
		// Paused/cancelled/shutting down: close the run, leave state for a re-run.
		_ = s.db.FinishStageRun(ctx, runID, false, json.RawMessage(`{"cancelled":true}`))
		return
	}
	if err != nil {
		_ = s.db.FinishStageRun(ctx, runID, false, metricsErr(err))
		s.setStatus(b.ID, state.StatusFailed, err.Error())
		return
	}
	if ferr := s.db.FinishStageRun(ctx, runID, true, result.Metrics); ferr != nil {
		return
	}
	s.advance(ctx, b, stage, result)
}

// execute runs the injected executor with a progress reporter that persists and
// publishes stage.progress.
func (s *Scheduler) execute(ctx context.Context, b store.Book, stage state.State) (StageResult, error) {
	report := func(done, total int) {
		_ = s.db.SetProgress(ctx, b.ID, string(stage), done, total)
		_ = s.hub.Publish("stage.progress", map[string]any{
			"book_id": b.ID, "stage": string(stage), "done": done, "total": total,
		})
	}
	return s.exec.Execute(ctx, b, stage, report)
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
		s.setStatus(b.ID, state.StatusFailed, err.Error())
		return
	}
	if status == state.StatusNeedsAttention {
		// Park: keep the state, flag needs_attention (audit unresolved after cap).
		s.setStatus(b.ID, state.StatusNeedsAttention, "audit failed after maximum fix attempts")
		return
	}

	// Preserve any status set concurrently (e.g. a pause during this stage).
	cur, err := s.db.GetBook(ctx, b.ID)
	if err != nil {
		return
	}
	if err := s.db.SetBookState(ctx, b.ID, string(next), cur.Status, ""); err != nil {
		return
	}
	s.publishState(b.ID, string(next), cur.Status)
}

// advanceWaypoints promotes queued/ready books (no lane, no executor) to their
// next state until none remain, so the machine never stalls on a waypoint. It
// loops until a pass advances nothing and returns that final, fresh book list so
// dispatch can act on it without re-querying.
func (s *Scheduler) advanceWaypoints(ctx context.Context) ([]store.Book, error) {
	for {
		books, err := s.db.ListBooks(ctx)
		if err != nil {
			return nil, err
		}
		advanced := false
		for _, b := range books {
			st := state.State(b.State)
			if b.Status != "" || !state.IsWaypoint(st) {
				continue
			}
			next, _, err := state.NextState(st, state.Outcome{})
			if err != nil {
				continue
			}
			if err := s.db.SetBookState(ctx, b.ID, string(next), "", ""); err != nil {
				continue
			}
			s.publishState(b.ID, string(next), "")
			advanced = true
		}
		if !advanced {
			return books, nil
		}
	}
}

// --- series lock ---

// lockHolders returns the set of book ids permitted to run an agent stage: the
// lowest-position unfinished book in each series, plus every seriesless book
// (each parallelizes freely). A book that has reached ready (or beyond) no longer
// holds its series; a parked (needs_attention) predecessor still does, so it
// blocks its successors' agent work until resumed or cancelled - by design.
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

// seriesLess orders two books of the same series by numeric position, then id.
func seriesLess(a, b store.Book) bool {
	pa, pb := parseSeriesPos(a.SeriesPos), parseSeriesPos(b.SeriesPos)
	if pa != pb {
		return pa < pb
	}
	return a.ID < b.ID
}

// parseSeriesPos extracts the leading number of a position string ("1", "2.5",
// "1-3.5" -> 1). Unparseable positions sort last (+Inf).
func parseSeriesPos(pos string) float64 {
	pos = strings.TrimSpace(pos)
	if pos == "" {
		return 1e18
	}
	end := 0
	for end < len(pos) {
		c := pos[end]
		if (c >= '0' && c <= '9') || c == '.' {
			end++
			continue
		}
		break
	}
	f, err := strconv.ParseFloat(pos[:end], 64)
	if err != nil {
		return 1e18
	}
	return f
}

// --- events ---

func (s *Scheduler) publishState(bookID int64, st, status string) {
	_ = s.hub.Publish("book.state", map[string]any{
		"book_id": bookID,
		"state":   st,
		"lane":    string(state.LaneOf(state.State(st))),
		"status":  status,
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
	next := queueStats{
		asr:    counts[state.LaneASR],
		agent:  counts[state.LaneAgent],
		mech:   counts[state.LaneMechanical],
		queued: queued,
	}
	if s.haveStats && next == s.lastStats {
		return
	}
	s.lastStats, s.haveStats = next, true
	_ = s.hub.Publish("queue.stats", map[string]any{
		"asr_active":        next.asr,
		"agent_active":      next.agent,
		"mechanical_active": next.mech,
		"queued":            next.queued,
	})
}

// setStatus writes an orthogonal status flag and publishes book.state.
func (s *Scheduler) setStatus(bookID int64, status state.Status, errMsg string) {
	ctx := context.Background()
	b, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return
	}
	if err := s.db.SetBookStatus(ctx, bookID, string(status), errMsg); err != nil {
		return
	}
	s.publishState(bookID, b.State, string(status))
}

func metricsErr(err error) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return b
}
