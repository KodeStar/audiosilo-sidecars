package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// SupervisorRuntimeSnapshot is a read-only view of scheduler ownership/capacity.
type SupervisorRuntimeSnapshot struct {
	ActiveBooks        map[int64]bool
	AgentActive        int
	AgentCapacity      int
	EligibleAgentBooks int
	EligibleAgentIDs   []int64
	AgentInvocations   int
	InvocationCapacity int
	InvocationsByBook  map[int64]int
	MaxAgentsPerBook   int
}

func (s *Scheduler) SupervisorRuntime(books []store.Book) SupervisorRuntimeSnapshot {
	out := SupervisorRuntimeSnapshot{ActiveBooks: map[int64]bool{}, AgentCapacity: s.agentCap, InvocationsByBook: map[int64]int{}}
	if runtime, ok := s.exec.(agentInvocationRuntime); ok {
		out.AgentInvocations, out.InvocationsByBook, out.InvocationCapacity = runtime.AgentInvocationRuntime()
	}
	if runtime, ok := s.exec.(agentFanoutRuntime); ok {
		out.MaxAgentsPerBook = runtime.AgentMaxPerBook()
	}
	s.mu.Lock()
	for id, ib := range s.inflight {
		out.ActiveBooks[id] = true
		if ib.lane == state.LaneAgent {
			out.AgentActive++
		}
	}
	s.mu.Unlock()
	holders := lockHolders(books)
	for _, b := range books {
		if out.ActiveBooks[b.ID] {
			continue
		}
		st := state.State(b.State)
		if state.LaneOf(st) == state.LaneAgent && state.CanStart(st, state.Status(b.Status), holders[b.ID]) {
			out.EligibleAgentBooks++
			out.EligibleAgentIDs = append(out.EligibleAgentIDs, b.ID)
		}
	}
	return out
}

// SupervisorApply executes the supervisor's small predefined playbook. The string
// action is mapped at the server seam so scheduler does not depend on supervisor.
func (s *Scheduler) SupervisorApply(ctx context.Context, action string, bookID int64, stage string) (string, error) {
	switch action {
	case "retry", "readmit":
		b, err := s.db.GetBook(ctx, bookID)
		if err != nil {
			return "", err
		}
		if b.Status == string(state.StatusFailed) || b.Status == string(state.StatusNeedsAttention) {
			if action == "readmit" && b.RetryAt != "" {
				due, parseErr := time.Parse(time.RFC3339Nano, b.RetryAt)
				if parseErr == nil && time.Now().Before(due) {
					return "existing transient readmission window retained until " + b.RetryAt, nil
				}
			}
			if err := s.readmit(ctx, b); err != nil {
				return "", err
			}
			return "book readmitted", nil
		}
		return "book was already admitted", nil
	case "requeue":
		s.notify()
		return "scheduler dispatch nudged", nil
	case "terminate_requeue":
		return s.supervisorTerminateRequeue(ctx, bookID)
	case "supersede_rerun":
		return s.supervisorSupersedeRerun(ctx, bookID, stage)
	case "stop_budget":
		return s.supervisorPark(ctx, bookID, state.ParkSupervisorBudget, "supervisor stopped the stage at its configured duration/token/cost limit")
	case "park_escalate":
		return s.supervisorPark(ctx, bookID, state.ParkSupervisorEscalated, "supervisor parked this book for operator review")
	case "reallocate":
		s.notify()
		return "idle configured capacity was offered queued work", nil
	default:
		return "", fmt.Errorf("unsupported supervisor action %q", action)
	}
}

func (s *Scheduler) supervisorTerminateRequeue(ctx context.Context, bookID int64) (string, error) {
	b, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	_, active := s.inflight[bookID]
	s.mu.Unlock()
	if active {
		s.cancelInflight(bookID)
		s.notify()
		return "stuck worker terminated; current stage will requeue", nil
	}
	runs, err := s.db.OpenStageRuns(ctx)
	if err != nil {
		return "", err
	}
	closed := false
	for _, r := range runs {
		if r.BookID == bookID {
			if err := s.db.FinishStageRun(ctx, r.ID, false, json.RawMessage(`{"interrupted":true,"supervisor":true}`)); err != nil {
				return "", err
			}
			closed = true
		}
	}
	if b.Status != "" {
		if err := s.db.SetBookStatus(ctx, b.ID, "", "", ""); err != nil {
			return "", err
		}
	}
	s.notify()
	if closed {
		return "orphaned database run closed and requeued", nil
	}
	return "no active invocation remained; scheduler nudged", nil
}

func (s *Scheduler) supervisorSupersedeRerun(ctx context.Context, bookID int64, stageName string) (string, error) {
	stage := state.State(stageName)
	if !state.IsStage(stage) || stage == state.Contributing {
		return "", ErrInvalidOp
	}
	b, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return "", err
	}
	if state.Order(state.State(b.State)) >= state.Order(state.Ready) {
		return "", ErrInvalidOp
	}
	runs, err := s.db.ListStageRuns(ctx, bookID)
	if err != nil {
		return "", err
	}
	for _, run := range runs {
		if run.Stage == string(state.Contributing) && run.Ok != nil && *run.Ok && !run.Superseded {
			return "", ErrInvalidOp
		}
	}
	s.cancelInflight(bookID)
	for _, candidate := range state.All() {
		if state.IsStage(candidate) && state.Order(candidate) >= state.Order(stage) {
			if err := s.db.SupersedeStageSuccesses(ctx, bookID, string(candidate)); err != nil {
				return "", err
			}
			_ = os.Remove(SentinelPath(b.WorkDir, string(candidate)))
		}
	}
	status, errMessage, parkCode := "", "", ""
	if b.Status == string(state.StatusPaused) {
		status, errMessage, parkCode = b.Status, b.Error, b.ParkCode
	}
	if err := s.db.SetBookState(ctx, bookID, string(stage), status, errMessage, parkCode); err != nil {
		return "", err
	}
	s.publishState(bookID, string(stage), status, errMessage, parkCode, "")
	s.notify()
	return "invalidated stage and later sentinels; rerun queued with released inputs/code", nil
}

func (s *Scheduler) supervisorPark(ctx context.Context, bookID int64, code state.ParkCode, reason string) (string, error) {
	b, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return "", err
	}
	if state.IsTerminal(state.State(b.State)) {
		return "", ErrInvalidOp
	}
	if err := s.db.SetBookStatus(ctx, bookID, string(state.StatusNeedsAttention), reason, string(code)); err != nil {
		return "", err
	}
	s.cancelInflight(bookID)
	s.publishState(bookID, b.State, string(state.StatusNeedsAttention), reason, string(code), "")
	return "book parked and escalated", nil
}
