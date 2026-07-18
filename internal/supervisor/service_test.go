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
	s := New(db, cfg, pricing.Table{Version: "test"}, nil, Hooks{Runtime: func() Runtime { return Runtime{ActiveBooks: map[int64]bool{}, AgentCapacity: 2} }, Apply: func(_ context.Context, a Action, i Incident) (string, error) {
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
