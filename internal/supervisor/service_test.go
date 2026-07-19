package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/pricing"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

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
	decision := Decide(Incident{Kind: IncidentNoProgress}, 1, s.policy)
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
