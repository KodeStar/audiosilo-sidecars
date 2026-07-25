package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/pricing"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

func failedRunMetrics(t *testing.T, errMsg string) json.RawMessage {
	t.Helper()
	m, err := json.Marshal(map[string]string{"error": errMsg})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

type fixedModel struct {
	decision ModelDecision
	usage    agent.Usage
	calls    int
}

type cancelUsageModel struct {
	started chan struct{}
}

func (m *cancelUsageModel) Info() ModelInfo {
	return ModelInfo{Backend: "claude", Model: "supervisor-test", ProviderReportsCost: true}
}
func (m *cancelUsageModel) Diagnose(ctx context.Context, _ ModelContext) (ModelDecision, agent.Usage, error) {
	close(m.started)
	<-ctx.Done()
	return ModelDecision{}, agent.Usage{Model: "supervisor-test", Input: 200, Output: 20, CostUSD: .04, CostReported: true}, ctx.Err()
}

func (m *fixedModel) Info() ModelInfo {
	return ModelInfo{Backend: "claude", Model: "supervisor-test", ProviderReportsCost: true}
}
func (m *fixedModel) Diagnose(context.Context, ModelContext) (ModelDecision, agent.Usage, error) {
	m.calls++
	return m.decision, m.usage, nil
}

func supervisorDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestParkedRecoveryFingerprintIgnoresRewrittenBookError proves change 1: the
// parked-recovery fingerprint is derived from the stable underlying stage-run error,
// not from book.Error (which every supervisor escalation rewrites). Two snapshots that
// differ ONLY in book.Error must fingerprint identically; a genuinely different
// underlying failure must fingerprint differently.
func TestParkedRecoveryFingerprintIgnoresRewrittenBookError(t *testing.T) {
	no := false
	underlying := "ffmpeg: Invalid data found when processing input"
	runs := func(errMsg string) []store.StageRun {
		return []store.StageRun{{ID: 7, Stage: "retranscribing", FinishedAt: "2026-07-19T09:50:07Z", Ok: &no, Metrics: failedRunMetrics(t, errMsg)}}
	}
	book := func(bookError string) store.Book {
		return store.Book{ID: 1, State: "retranscribing", Status: string(state.StatusNeedsAttention), Error: bookError, ParkCode: string(state.ParkQANoConverge)}
	}
	find := func(b store.Book, r []store.StageRun) Incident {
		for _, i := range Classify(Snapshot{Book: b, Runs: r}, Policy{MaxAttempts: 3}) {
			if i.Kind == IncidentParkedRecovery {
				return i
			}
		}
		t.Fatalf("no parked_recovery incident for %+v", b)
		return Incident{}
	}
	genuine := find(book("stage failed: "+underlying), runs(underlying))
	// A later recovery cycle: park_escalate rewrote book.Error to nested supervisor prose,
	// but the underlying stage-run error is byte-for-byte identical.
	rewritten := find(book("supervisor: parked book requires a bounded recovery plan: park code qa_no_converge; stage failed: "+underlying), runs(underlying))
	if genuine.Fingerprint == "" || genuine.Fingerprint != rewritten.Fingerprint {
		t.Fatalf("fingerprint drifted with a rewritten book.Error: %q vs %q", genuine.Fingerprint, rewritten.Fingerprint)
	}
	other := find(book("stage failed: other"), runs("ffmpeg: No such file or directory"))
	if other.Fingerprint == genuine.Fingerprint {
		t.Fatalf("a distinct underlying error collapsed into the same fingerprint")
	}
}

// TestPingPongRecoveryEscalatesByCapAndStopsRetrying reproduces the production incident:
// a deterministically-failing stage, alternating park/readmit cycles, with book.Error
// rewritten each escalation by the supervisor message builder. With the fix, the book is
// automatically retried at most MaxAttempts times, then escalates for operator review and
// stops retrying.
func TestPingPongRecoveryEscalatesByCapAndStopsRetrying(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	_ = db.EnsureBatch(ctx, "pingpong", time.Now())
	b, _ := db.CreateBook(ctx, store.NewBook{BatchID: "pingpong", SourcePath: "/pp", WorkDir: t.TempDir(), Title: "PingPong", State: "retranscribing"})
	underlying := "ffmpeg: deterministic conversion failure"
	metrics := failedRunMetrics(t, underlying)
	first, _ := db.StartStageRun(ctx, b.ID, "retranscribing", 1)
	_ = db.FinishStageRun(ctx, first, false, metrics)
	_ = db.SetBookStatus(ctx, b.ID, string(state.StatusNeedsAttention), underlying, string(state.ParkQANoConverge))

	cfg := config.Default().Supervisor
	cfg.AutomaticActions = true
	cfg.MaxAttempts = 3
	cfg.StaleMinutes, cfg.NoProgressMinutes, cfg.MaxStageMinutes = 999, 999, 999

	var mu sync.Mutex
	var applied []Action
	attempt := 1
	s := New(db, cfg, pricing.Table{Version: "test"}, nil, Hooks{
		Runtime: func([]store.Book) Runtime { return Runtime{ActiveBooks: map[int64]bool{}} },
		Apply: func(_ context.Context, a Action, i Incident) (string, error) {
			mu.Lock()
			applied = append(applied, a)
			mu.Unlock()
			switch a {
			case ActionRetry, ActionReadmit:
				// Readmit: the stage runs and fails identically; the pipeline leaves the
				// failed run and a failed book (genuine underlying error preserved).
				attempt++
				id, _ := db.StartStageRun(ctx, i.BookID, "retranscribing", attempt)
				_ = db.FinishStageRun(ctx, id, false, metrics)
				_ = db.SetBookStatus(ctx, i.BookID, string(state.StatusFailed), underlying, "")
			case ActionParkEscalate:
				// Reproduce supervisorPark: nested supervisor prose becomes the new
				// book.Error and the park code becomes supervisor_escalated.
				detail := i.Diagnosis
				if len(i.Evidence) > 0 {
					detail += ": " + strings.Join(i.Evidence, "; ")
				}
				_ = db.SetBookStatus(ctx, i.BookID, string(state.StatusNeedsAttention), "supervisor: "+detail, string(state.ParkSupervisorEscalated))
			}
			return "simulated", nil
		},
	})
	for n := 0; n < 16; n++ {
		s.check(ctx, "pingpong")
	}

	mu.Lock()
	defer mu.Unlock()
	retries := 0
	for _, a := range applied {
		if a == ActionRetry || a == ActionReadmit {
			retries++
		}
	}
	if retries != cfg.MaxAttempts {
		t.Fatalf("applied %d automatic retries, want the cap of %d; actions=%v", retries, cfg.MaxAttempts, applied)
	}
	if len(applied) == 0 || applied[len(applied)-1] != ActionParkEscalate {
		t.Fatalf("recovery loop did not terminate in escalation; actions=%v", applied)
	}
	runs, err := db.RecentSupervisorRuns(ctx, "pingpong", 50)
	if err != nil {
		t.Fatal(err)
	}
	capped := false
	for _, r := range runs {
		if r.SelectedAction == string(ActionParkEscalate) && r.ApprovalRequired && strings.Contains(string(r.Evidence), "auto-recovery cap") {
			capped = true
		}
	}
	if !capped {
		t.Fatalf("no cap-triggered escalation with approval recorded; runs=%d", len(runs))
	}
	got, _ := db.GetBook(ctx, b.ID)
	if got.Status != string(state.StatusNeedsAttention) {
		t.Fatalf("book not left parked for review: status=%q park=%q", got.Status, got.ParkCode)
	}
}

// seedAutoRecovery records a completed automatic retry supervisor run for a book under a
// distinct incident key, so the cross-Kind cap can be exercised without relying on the
// per-family attempt count.
func seedAutoRecovery(t *testing.T, db *store.DB, batch string, bookID int64, key, startedAt string) {
	t.Helper()
	bid := bookID
	if _, err := db.StartSupervisorRun(context.Background(), store.SupervisorRun{IncidentKey: key, BatchID: batch, BookID: &bid,
		Trigger: "seed", Diagnosis: "prior recovery", Evidence: json.RawMessage(`[]`), SelectedAction: "retry",
		Automatic: true, State: "completed", StartedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
}

// TestCrossKindAutoRecoveryCapForcesEscalation proves change 2: recoveries recorded under
// DIFFERENT kinds/fingerprints for the same book still count toward the per-book cap, so a
// fresh incident escalates instead of retrying once the cap is reached.
func TestCrossKindAutoRecoveryCapForcesEscalation(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	_ = db.EnsureBatch(ctx, "crosskind", time.Now())
	b, _ := db.CreateBook(ctx, store.NewBook{BatchID: "crosskind", SourcePath: "/ck", WorkDir: t.TempDir(), Title: "CrossKind", State: "retranscribing"})
	now := time.Now().UTC()
	for n := 0; n < 3; n++ {
		key := fmt.Sprintf("kind-%d/%d/retranscribing/%d/fp-%d", n, b.ID, n, n)
		seedAutoRecovery(t, db, "crosskind", b.ID, key, store.Timestamp(now.Add(-time.Duration(n+1)*time.Minute)))
	}
	id, _ := db.StartStageRun(ctx, b.ID, "retranscribing", 1)
	_ = db.FinishStageRun(ctx, id, false, failedRunMetrics(t, "ffmpeg: deterministic conversion failure"))
	_ = db.SetBookStatus(ctx, b.ID, string(state.StatusNeedsAttention), "ffmpeg: deterministic conversion failure", string(state.ParkQANoConverge))

	cfg := config.Default().Supervisor
	cfg.AutomaticActions = true
	cfg.MaxAttempts = 3
	cfg.StaleMinutes, cfg.NoProgressMinutes, cfg.MaxStageMinutes = 999, 999, 999
	var applied []Action
	s := New(db, cfg, pricing.Table{Version: "test"}, nil, Hooks{
		Runtime: func([]store.Book) Runtime { return Runtime{ActiveBooks: map[int64]bool{}} },
		Apply: func(_ context.Context, a Action, _ Incident) (string, error) {
			applied = append(applied, a)
			return "simulated", nil
		},
	})
	s.check(ctx, "crosskind")
	if len(applied) != 1 || applied[0] != ActionParkEscalate {
		t.Fatalf("cross-Kind cap did not force escalation; actions=%v", applied)
	}
}

// TestAgedRecoveriesDoNotConsumeTheAutoRecoveryCap proves the window boundary: recoveries
// older than the rolling window do not count, so a distinct failure after a human
// intervention (and enough elapsed time) still receives a fresh automatic attempt.
func TestAgedRecoveriesDoNotConsumeTheAutoRecoveryCap(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	_ = db.EnsureBatch(ctx, "aged", time.Now())
	b, _ := db.CreateBook(ctx, store.NewBook{BatchID: "aged", SourcePath: "/aged", WorkDir: t.TempDir(), Title: "Aged", State: "retranscribing"})
	old := time.Now().UTC().Add(-48 * time.Hour)
	for n := 0; n < 3; n++ {
		key := fmt.Sprintf("kind-%d/%d/retranscribing/%d/fp-%d", n, b.ID, n, n)
		seedAutoRecovery(t, db, "aged", b.ID, key, store.Timestamp(old.Add(-time.Duration(n)*time.Minute)))
	}
	id, _ := db.StartStageRun(ctx, b.ID, "retranscribing", 1)
	_ = db.FinishStageRun(ctx, id, false, failedRunMetrics(t, "ffmpeg: a genuinely new failure"))
	_ = db.SetBookStatus(ctx, b.ID, string(state.StatusNeedsAttention), "ffmpeg: a genuinely new failure", string(state.ParkQANoConverge))

	cfg := config.Default().Supervisor
	cfg.AutomaticActions = true
	cfg.MaxAttempts = 3
	cfg.StaleMinutes, cfg.NoProgressMinutes, cfg.MaxStageMinutes = 999, 999, 999
	var applied []Action
	s := New(db, cfg, pricing.Table{Version: "test"}, nil, Hooks{
		Runtime: func([]store.Book) Runtime { return Runtime{ActiveBooks: map[int64]bool{}} },
		Apply: func(_ context.Context, a Action, _ Incident) (string, error) {
			applied = append(applied, a)
			return "simulated", nil
		},
	})
	s.check(ctx, "aged")
	if len(applied) != 1 || applied[0] != ActionRetry {
		t.Fatalf("aged recoveries should not consume the cap; actions=%v", applied)
	}
}

// TestModelLaneRecoveryObeysAutoRecoveryCap proves Fix 2: a book that routed to the MODEL lane
// (supervisor_escalated -> ask_model) whose durable auto-recovery cap is already exhausted must
// NOT resume the ping-pong when the model recommends a retry - the model-recommended recovery is
// refused and recorded as the same cap escalation (park_escalate + approval + cap evidence) the
// deterministic lane produces, so the model lane cannot smuggle a capped book back into recovery.
func TestModelLaneRecoveryObeysAutoRecoveryCap(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	_ = db.EnsureBatch(ctx, "modelcap", time.Now())
	b, _ := db.CreateBook(ctx, store.NewBook{BatchID: "modelcap", SourcePath: "/mc", WorkDir: t.TempDir(), Title: "ModelCap", State: "retranscribing"})
	// Exhaust the cap with prior automatic recoveries under distinct incident keys.
	now := time.Now().UTC()
	for n := 0; n < 3; n++ {
		key := fmt.Sprintf("kind-%d/%d/retranscribing/%d/fp-%d", n, b.ID, n, n)
		seedAutoRecovery(t, db, "modelcap", b.ID, key, store.Timestamp(now.Add(-time.Duration(n+1)*time.Minute)))
	}
	// Park the book supervisor_escalated so the parked-recovery incident routes to the model lane.
	id, _ := db.StartStageRun(ctx, b.ID, "retranscribing", 1)
	_ = db.FinishStageRun(ctx, id, false, failedRunMetrics(t, "ffmpeg: deterministic conversion failure"))
	_ = db.SetBookStatus(ctx, b.ID, string(state.StatusNeedsAttention), "supervisor: prior", string(state.ParkSupervisorEscalated))

	cfg := config.Default().Supervisor
	cfg.AutomaticActions, cfg.ModelAssisted, cfg.ModelAutomaticActions = true, true, true
	cfg.MaxAttempts = 3
	cfg.StaleMinutes, cfg.NoProgressMinutes, cfg.MaxStageMinutes = 999, 999, 999
	m := &fixedModel{decision: ModelDecision{Diagnosis: "just retry it", Confidence: .8, Evidence: []string{"bounded"}, RecommendedAction: ActionRetry, SuggestedRetryLimit: 1}, usage: agent.Usage{Model: "supervisor-test", CostReported: true}}
	var applied []Action
	s := New(db, cfg, pricing.Table{Version: "test"}, m, Hooks{
		Runtime: func([]store.Book) Runtime { return Runtime{ActiveBooks: map[int64]bool{}} },
		Apply: func(_ context.Context, a Action, _ Incident) (string, error) {
			applied = append(applied, a)
			return "simulated", nil
		},
	})
	s.check(ctx, "modelcap")

	if len(applied) != 0 {
		t.Fatalf("a capped model-recommended recovery was applied; actions=%v", applied)
	}
	if m.calls != 1 {
		t.Fatalf("model consulted %d times, want exactly 1", m.calls)
	}
	runs, err := db.RecentSupervisorRuns(ctx, "modelcap", 50)
	if err != nil {
		t.Fatal(err)
	}
	capped := false
	for _, r := range runs {
		if r.Decision == "model_assisted" && r.SelectedAction == string(ActionParkEscalate) && r.ApprovalRequired &&
			!r.Automatic && strings.Contains(string(r.Evidence), "auto-recovery cap") {
			capped = true
		}
	}
	if !capped {
		t.Fatalf("model lane did not record the cap escalation; runs=%+v", runs)
	}
}

func TestSimulatedMultiBookRecoveryAndEscalation(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	_ = db.EnsureBatch(ctx, "simulation", time.Now())
	b1, _ := db.CreateBook(ctx, store.NewBook{BatchID: "simulation", SourcePath: "/orphan", WorkDir: t.TempDir(), Title: "Orphan", State: "fact_pass"})
	_, _ = db.StartStageRun(ctx, b1.ID, "fact_pass", 1)
	b2, _ := db.CreateBook(ctx, store.NewBook{BatchID: "simulation", SourcePath: "/repeat", WorkDir: t.TempDir(), Title: "Repeat", State: "auditing"})
	for n := 1; n <= 2; n++ {
		id, _ := db.StartStageRun(ctx, b2.ID, "auditing", n)
		_ = db.FinishStageRun(ctx, id, false, json.RawMessage(`{"error":"audit validation failed in the same way"}`))
	}
	cfg := config.Default().Supervisor
	cfg.AutomaticActions = true
	cfg.StaleMinutes = 999
	cfg.NoProgressMinutes = 999
	cfg.MaxStageMinutes = 999
	var mu sync.Mutex
	actions := map[int64][]Action{}
	s := New(db, cfg, pricing.Table{Version: "test"}, nil, Hooks{Runtime: func([]store.Book) Runtime { return Runtime{ActiveBooks: map[int64]bool{}, AgentCapacity: 2} }, Apply: func(_ context.Context, a Action, i Incident) (string, error) {
		mu.Lock()
		actions[i.BookID] = append(actions[i.BookID], a)
		mu.Unlock()
		return "simulated", nil
	}})
	s.check(ctx, "simulation")
	mu.Lock()
	defer mu.Unlock()
	if len(actions[b1.ID]) == 0 || actions[b1.ID][0] != ActionTerminateRequeue {
		t.Fatalf("orphan actions=%v", actions[b1.ID])
	}
	if len(actions[b2.ID]) == 0 || actions[b2.ID][0] != ActionParkEscalate {
		t.Fatalf("repeat actions=%v", actions[b2.ID])
	}
	runs, err := db.RecentSupervisorRuns(ctx, "simulation", 20)
	if err != nil || len(runs) < 2 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}

func TestLivenessFailsWhenAnyOfSeveralChildProcessesDisappears(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	if err := db.EnsureBatch(ctx, "children", time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBook(ctx, store.NewBook{BatchID: "children", SourcePath: "/children", WorkDir: t.TempDir(), Title: "Children", State: "fact_pass"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartStageRun(ctx, b.ID, "fact_pass", 1); err != nil {
		t.Fatal(err)
	}
	alive, err := db.StartAgentInvocation(ctx, b.ID, "fact_pass", "chunk-1", "claude", "sonnet")
	if err != nil {
		t.Fatal(err)
	}
	missing, err := db.StartAgentInvocation(ctx, b.ID, "fact_pass", "chunk-2", "claude", "sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAgentInvocationProcess(ctx, alive, os.Getpid(), true); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAgentInvocationProcess(ctx, missing, 1<<30, true); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default().Supervisor
	cfg.StaleMinutes, cfg.NoProgressMinutes, cfg.MaxStageMinutes = 999, 999, 999
	s := New(db, cfg, pricing.Table{Version: "test"}, nil, Hooks{Runtime: func([]store.Book) Runtime {
		return Runtime{ActiveBooks: map[int64]bool{b.ID: true}, AgentActive: 1, AgentCapacity: 1, AgentInvocations: 2, InvocationCapacity: 2, InvocationsByBook: map[int64]int{b.ID: 2}, MaxAgentsPerBook: 2}
	}})
	s.check(ctx, "children")
	runs, err := db.RecentSupervisorRuns(ctx, "children", 10)
	if err != nil || len(runs) == 0 {
		t.Fatalf("supervisor runs=%+v err=%v", runs, err)
	}
	if runs[0].Diagnosis != "recorded invocation process has disappeared" {
		t.Fatalf("diagnosis=%q", runs[0].Diagnosis)
	}
}

func TestModelBudgetsPerBookAndBatch(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	_ = db.EnsureBatch(ctx, "budget", time.Now())
	b, _ := db.CreateBook(ctx, store.NewBook{BatchID: "budget", SourcePath: "/b", WorkDir: t.TempDir(), Title: "B"})
	bid := b.ID
	one := 1.0
	_, _ = db.StartSupervisorRun(ctx, store.SupervisorRun{BatchID: "budget", BookID: &bid, Trigger: "old", Diagnosis: "old", Evidence: json.RawMessage(`[]`), State: "completed", Model: "x", ProviderCostUSD: &one, ProviderCostComplete: true})
	cfg := config.Default().Supervisor
	cfg.PerBookBudgetUSD = 1
	cfg.OverallBatchBudgetUSD = 5
	s := New(db, cfg, pricing.Table{Version: "v"}, nil, Hooks{})
	ok, reason := s.modelBudgetAllows(ctx, "budget", &bid)
	if ok || reason != "per-book supervisor budget reached" {
		t.Fatalf("ok=%v reason=%q", ok, reason)
	}
	cfg.PerBookBudgetUSD = 2
	cfg.OverallBatchBudgetUSD = 1
	s = New(db, cfg, pricing.Table{Version: "v"}, nil, Hooks{})
	ok, reason = s.modelBudgetAllows(ctx, "budget", &bid)
	if ok || reason != "overall batch supervisor budget reached" {
		t.Fatalf("ok=%v reason=%q", ok, reason)
	}
}

func TestAskSupervisorPersistsReportedAndEstimatedCosts(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	_ = db.EnsureBatch(ctx, "ask", time.Now())
	b, _ := db.CreateBook(ctx, store.NewBook{BatchID: "ask", SourcePath: "/ask", WorkDir: t.TempDir(), Title: "Ask"})
	m := &fixedModel{decision: ModelDecision{Diagnosis: "safe to observe", Confidence: .8, Evidence: []string{"bounded"}, RecommendedAction: ActionObserve, SuggestedRetryLimit: 1, SuggestedTerminationLimit: 0}, usage: agent.Usage{Model: "supervisor-test", Input: 1000, Output: 100, CacheRead: 50, CostUSD: .02, CostReported: true, Turns: 2}}
	cfg := config.Default().Supervisor
	cfg.ModelAssisted = true
	prices := pricing.Table{Version: "prices-v1", Rates: map[string]pricing.Rate{"claude/supervisor-test": {InputUSDPerMillion: 1, OutputUSDPerMillion: 2, CachedInputUSDPerMillion: .5}}}
	s := New(db, cfg, prices, m, Hooks{})
	run, err := s.Ask(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ProviderCostUSD == nil || *run.ProviderCostUSD != .02 || !run.ProviderCostComplete || run.EstimatedAPICostUSD == nil || !run.EstimateComplete || run.SuggestedRetryLimit != 1 {
		t.Fatalf("run=%+v", run)
	}
	if m.calls != 1 {
		t.Fatalf("calls=%d", m.calls)
	}
}

func TestCancelledAskStillFinalizesUsageLedger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db := supervisorDB(t)
	if err := db.EnsureBatch(ctx, "cancel", time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBook(ctx, store.NewBook{BatchID: "cancel", SourcePath: "/cancel", WorkDir: t.TempDir(), Title: "Cancel"})
	if err != nil {
		t.Fatal(err)
	}
	m := &cancelUsageModel{started: make(chan struct{})}
	cfg := config.Default().Supervisor
	cfg.ModelAssisted = true
	s := New(db, cfg, pricing.Table{Version: "test"}, m, Hooks{})
	done := make(chan error, 1)
	go func() {
		_, askErr := s.Ask(ctx, b.ID)
		done <- askErr
	}()
	<-m.started
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	runs, err := db.RecentSupervisorRuns(context.Background(), "cancel", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].CompletedAt == "" || runs[0].State != "failed" || runs[0].InputTokens != 200 || runs[0].ProviderCostUSD == nil || *runs[0].ProviderCostUSD != .04 {
		t.Fatalf("cancelled model call was not fully accounted: %+v", runs)
	}
}

func TestModelContextDistinguishesReportedAndEstimatedCost(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	if err := db.EnsureBatch(ctx, "context-cost", time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBook(ctx, store.NewBook{BatchID: "context-cost", SourcePath: "/context", WorkDir: t.TempDir(), Title: "Context"})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := db.StartStageRun(ctx, b.ID, "fact_pass", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddOpenStageRunUsageDetailed(ctx, b.ID, "fact_pass", "codex-model", 100, 20, 10, 0, false, .03, true); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(ctx, runID, true, nil); err != nil {
		t.Fatal(err)
	}
	runs, err := db.ListStageRuns(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	s := New(db, config.Default().Supervisor, pricing.Table{Version: "test"}, nil, Hooks{})
	modelContext, err := s.modelContext(ctx, Incident{BookID: b.ID}, runs, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if len(modelContext.Attempts) != 1 || modelContext.Attempts[0].ProviderCostUSD != nil || modelContext.Attempts[0].ProviderCostComplete || modelContext.Attempts[0].EstimatedAPICostUSD == nil || *modelContext.Attempts[0].EstimatedAPICostUSD != .03 || !modelContext.Attempts[0].EstimateComplete {
		t.Fatalf("cost availability was misrepresented: %+v", modelContext.Attempts)
	}
}

func TestAmbiguousIncidentInvokesModelOncePerEvent(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	if err := db.EnsureBatch(ctx, "event", time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBook(ctx, store.NewBook{BatchID: "event", SourcePath: "/event", WorkDir: t.TempDir(), Title: "Event", State: "fact_pass"})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := db.StartStageRun(ctx, b.ID, "fact_pass", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(ctx, runID, false, json.RawMessage(`{"error":"novel structured failure"}`)); err != nil {
		t.Fatal(err)
	}
	m := &fixedModel{decision: ModelDecision{Diagnosis: "observe the novel failure", Confidence: .7, Evidence: []string{"one bounded attempt"}, RecommendedAction: ActionObserve, SuggestedRetryLimit: 1}, usage: agent.Usage{Model: "supervisor-test", Input: 20, Output: 10, CostReported: true}}
	cfg := config.Default().Supervisor
	cfg.ModelAssisted = true
	s := New(db, cfg, pricing.Table{Version: "test"}, m, Hooks{})
	s.CheckNow(ctx)
	s.CheckNow(ctx)
	if m.calls != 1 {
		t.Fatalf("model calls=%d, want one event-driven call", m.calls)
	}
	runs, err := db.RecentSupervisorRuns(ctx, "event", 10)
	if err != nil || len(runs) != 1 || runs[0].Decision != "model_assisted" {
		t.Fatalf("supervisor runs=%+v err=%v", runs, err)
	}
}

func TestModelFallbackRequiresExplicitPreapproval(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	if err := db.EnsureBatch(ctx, "fallback", time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBook(ctx, store.NewBook{BatchID: "fallback", SourcePath: "/fallback", WorkDir: t.TempDir(), Title: "Fallback"})
	if err != nil {
		t.Fatal(err)
	}
	m := &fixedModel{decision: ModelDecision{Diagnosis: "backend might be down", Confidence: .6, Evidence: []string{"bounded evidence"}, RecommendedAction: ActionFallbackBackend, SuggestedRetryLimit: 1}, usage: agent.Usage{Model: "supervisor-test", CostReported: true}}
	cfg := config.Default().Supervisor
	cfg.ModelAssisted = true
	cfg.AutomaticActions = true
	cfg.ModelAutomaticActions = true
	cfg.AllowBackendFailover = false
	s := New(db, cfg, pricing.Table{Version: "test"}, m, Hooks{})
	run, err := s.Ask(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "approval_required" || !run.ApprovalRequired || run.Automatic {
		t.Fatalf("fallback decision did not fail closed: %+v", run)
	}
}

func TestDisabledSupervisorDoesNotInspectOrMutateExistingProcessing(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	if err := db.EnsureBatch(ctx, "disabled", time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBook(ctx, store.NewBook{BatchID: "disabled", SourcePath: "/disabled", WorkDir: t.TempDir(), Title: "Disabled", State: "fact_pass"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartStageRun(ctx, b.ID, "fact_pass", 1); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default().Supervisor
	cfg.Enabled = false
	applyCalls := 0
	s := New(db, cfg, pricing.Table{Version: "test"}, nil, Hooks{Apply: func(context.Context, Action, Incident) (string, error) {
		applyCalls++
		return "unexpected", nil
	}})
	s.Run(ctx)
	if applyCalls != 0 {
		t.Fatalf("disabled supervisor applied %d actions", applyCalls)
	}
	runs, err := db.RecentSupervisorRuns(ctx, "disabled", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("disabled supervisor persisted incidents: %+v", runs)
	}
	got, err := db.GetBook(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "fact_pass" || got.Status != "" {
		t.Fatalf("book changed with supervision disabled: %+v", got)
	}
}

func TestUnavailableModelKeepsDeterministicEscalation(t *testing.T) {
	cfg := config.Default().Supervisor
	cfg.ModelAssisted = true
	s := New(supervisorDB(t), cfg, pricing.Table{Version: "test"}, nil, Hooks{})
	decision := Decide(Incident{Kind: IncidentNoProgress}, 1, 0, s.policy)
	if decision.Action != ActionParkEscalate || !decision.ApprovalRequired || decision.Automatic {
		t.Fatalf("unavailable model suppressed deterministic escalation: %+v", decision)
	}
}

func TestArtifactStatusRejectsStructurallyInvalidSentinelAndJSONArtifact(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "_done"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "_done", "validating.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "validation_report.json"), []byte(`not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := true
	statuses := artifactStatuses(store.Book{WorkDir: work}, []store.StageRun{{Stage: "validating", Ok: &ok}})
	if len(statuses) != 2 || statuses[0].Valid || statuses[1].Valid {
		t.Fatalf("artifact statuses=%+v", statuses)
	}
}

func TestArtifactStatusIgnoresDoneBookSplitSentinelRemovedByPurge(t *testing.T) {
	ok := true
	statuses := artifactStatuses(store.Book{State: "done", WorkDir: t.TempDir()}, []store.StageRun{{Stage: "splitting", Ok: &ok}})
	if len(statuses) != 0 {
		t.Fatalf("intentional done-book purge reported as incident: %+v", statuses)
	}
}

func TestArtifactStatusIgnoresSentinelRemovedForCurrentStageRerun(t *testing.T) {
	// The scheduler deliberately removes a stage's old sentinel when a loop enters
	// that stage again. A prior successful audit therefore has no auditing sentinel
	// while the fresh audit is queued/running; that is expected, not corruption.
	ok := true
	work := t.TempDir()
	runs := []store.StageRun{
		{ID: 408, Stage: "auditing", FinishedAt: "2026-07-19T09:50:07Z", Ok: &ok},
		{ID: 411, Stage: "auditing", StartedAt: "2026-07-19T09:52:25Z"},
	}
	statuses := artifactStatuses(store.Book{State: "auditing", WorkDir: work}, runs)
	if len(statuses) != 0 {
		t.Fatalf("current audit rerun reported its intentionally absent sentinel: %+v", statuses)
	}
}

func TestArtifactStatusStillChecksCompletedEarlierStagesDuringRerun(t *testing.T) {
	// Skipping current-stage history must not suppress validation of genuinely
	// completed prerequisites. With no files present, validating remains invalid.
	ok := true
	runs := []store.StageRun{
		{ID: 410, Stage: "validating", FinishedAt: "2026-07-19T09:52:25Z", Ok: &ok},
		{ID: 411, Stage: "auditing", StartedAt: "2026-07-19T09:52:25Z"},
	}
	statuses := artifactStatuses(store.Book{State: "auditing", WorkDir: t.TempDir()}, runs)
	if len(statuses) != 2 || statuses[0].Stage != "validating" || statuses[0].Valid || statuses[1].Valid {
		t.Fatalf("completed prerequisite statuses = %+v", statuses)
	}
}
