package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/pricing"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

var ErrModelDisabled = errors.New("model-assisted supervision is disabled")

type Runtime struct {
	ActiveBooks        map[int64]bool `json:"active_books"`
	AgentActive        int            `json:"agent_active"`
	AgentCapacity      int            `json:"agent_capacity"`
	EligibleAgentBooks int            `json:"eligible_agent_books"`
	EligibleAgentIDs   []int64        `json:"eligible_agent_book_ids,omitempty"`
}

type Hooks struct {
	Runtime func() Runtime
	Apply   func(context.Context, Action, Incident) (string, error)
	Publish func(eventType string, bookID int64, payload any)
}

type Status struct {
	State                string  `json:"state"`
	Enabled              bool    `json:"enabled"`
	AutomaticActions     bool    `json:"automatic_actions"`
	ModelAssisted        bool    `json:"model_assisted"`
	ModelAvailable       bool    `json:"model_available"`
	AllowBackendFailover bool    `json:"allow_backend_failover"`
	LastCheckAt          string  `json:"last_check_at,omitempty"`
	LastError            string  `json:"last_error,omitempty"`
	Runtime              Runtime `json:"runtime"`
}

type Service struct {
	db      *store.DB
	cfg     config.SupervisorConfig
	pricing pricing.Table
	model   Model
	hooks   Hooks
	policy  Policy

	checkMu   sync.Mutex
	mu        sync.Mutex
	modelMu   sync.Mutex
	lastCheck time.Time
	lastErr   string
}

func New(db *store.DB, cfg config.SupervisorConfig, prices pricing.Table, model Model, hooks Hooks) *Service {
	return &Service{db: db, cfg: cfg, pricing: prices, model: model, hooks: hooks, policy: Policy{
		StaleAfter:       time.Duration(cfg.StaleMinutes) * time.Minute,
		NoProgressAfter:  time.Duration(cfg.NoProgressMinutes) * time.Minute,
		MaxStageDuration: time.Duration(cfg.MaxStageMinutes) * time.Minute,
		MaxAttempts:      cfg.MaxAttempts, MaxErrorRepeats: cfg.MaxErrorRepeats,
		MaxStageTokens: cfg.MaxStageTokens, MaxStageCostUSD: cfg.MaxStageCostUSD, AttemptGrowthFactor: cfg.AttemptGrowthFactor,
		AutomaticActions: cfg.AutomaticActions, ModelAssisted: cfg.ModelAssisted,
		ModelAutomaticActions: cfg.ModelAutomaticActions, AllowBackendFailover: cfg.AllowBackendFailover,
	}}
}

func (s *Service) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	s.check(ctx, "startup")
	t := time.NewTicker(time.Duration(s.cfg.IntervalSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.check(ctx, "health_tick")
		}
	}
}

func (s *Service) CheckNow(ctx context.Context) {
	if s.cfg.Enabled {
		s.check(ctx, "manual_check")
	}
}

func (s *Service) check(ctx context.Context, trigger string) {
	if !s.checkMu.TryLock() {
		return
	}
	defer s.checkMu.Unlock()
	books, err := s.db.ListBooks(ctx)
	if err != nil {
		s.setCheck(err)
		return
	}
	runtime := Runtime{ActiveBooks: map[int64]bool{}}
	if s.hooks.Runtime != nil {
		runtime = s.hooks.Runtime()
		if runtime.ActiveBooks == nil {
			runtime.ActiveBooks = map[int64]bool{}
		}
	}
	for _, book := range books {
		if ctx.Err() != nil {
			return
		}
		runs, rerr := s.db.ListStageRuns(ctx, book.ID)
		if rerr != nil {
			s.setCheck(rerr)
			return
		}
		eligibleCount := 0
		if len(runtime.EligibleAgentIDs) > 0 && book.ID == runtime.EligibleAgentIDs[0] {
			eligibleCount = runtime.EligibleAgentBooks
		}
		snap := Snapshot{Now: time.Now().UTC(), Book: book, Runs: runs, RuntimeActive: runtime.ActiveBooks[book.ID],
			Artifacts: artifactStatuses(book, runs), AgentActive: runtime.AgentActive, AgentCapacity: runtime.AgentCapacity, EligibleAgentBooks: eligibleCount}
		for i := range runs {
			if runs[i].FinishedAt == "" && runs[i].ProcessActive && runs[i].ProcessID > 0 {
				alive := processAlive(runs[i].ProcessID)
				snap.ProcessAlive = &alive
				break
			}
		}
		if incident, ok := primaryIncident(Classify(snap, s.policy)); ok {
			s.handleIncident(ctx, trigger, incident, runs, runtime)
		}
	}
	s.mu.Lock()
	s.lastCheck, s.lastErr = time.Now().UTC(), ""
	s.mu.Unlock()
}

func primaryIncident(incidents []Incident) (Incident, bool) {
	if len(incidents) == 0 {
		return Incident{}, false
	}
	priority := map[IncidentKind]int{
		IncidentMissingProcess: 100, IncidentStaleHeartbeat: 95,
		IncidentCostLimit: 90, IncidentTokenLimit: 90, IncidentDurationLimit: 90,
		IncidentAuthentication: 85, IncidentBackendUnavailable: 84, IncidentRateLimit: 83,
		IncidentRepeatedError: 80, IncidentNonConvergingQA: 78, IncidentNonConvergingAudit: 78,
		IncidentArtifactInvalid: 75, IncidentNoProgress: 70, IncidentUnclassified: 60,
		IncidentSlotInefficiency: 10,
	}
	best := incidents[0]
	for _, candidate := range incidents[1:] {
		if priority[candidate.Kind] > priority[best.Kind] {
			best = candidate
		}
	}
	return best, true
}

func (s *Service) setCheck(err error) {
	s.mu.Lock()
	s.lastCheck, s.lastErr = time.Now().UTC(), err.Error()
	s.mu.Unlock()
}

func (s *Service) handleIncident(ctx context.Context, trigger string, i Incident, runs []store.StageRun, runtime Runtime) {
	key := incidentKey(i)
	if seen, err := s.db.HasIncident(ctx, key); err != nil || seen {
		return
	}
	attempts := 0
	for _, r := range runs {
		if r.Stage == i.Stage {
			attempts++
		}
	}
	d := Decide(i, attempts, s.policy)
	evidence, _ := json.Marshal(i.Evidence)
	bookID := i.BookID
	var stageRunID *int64
	if i.StageRunID > 0 {
		v := i.StageRunID
		stageRunID = &v
	}
	r := store.SupervisorRun{IncidentKey: key, BatchID: i.BatchID, BookID: &bookID, StageRunID: stageRunID, Trigger: trigger,
		Diagnosis: i.Diagnosis, Confidence: 1, Evidence: evidence, Decision: string(i.Kind), SelectedAction: string(d.Action),
		SuggestedRetryLimit: d.RetryLimit, SuggestedTerminationLimit: d.TerminationLimit,
		Automatic: d.Automatic, ApprovalRequired: d.ApprovalRequired, State: "decided", PricingVersion: s.pricing.Version}
	id, err := s.db.StartSupervisorRun(ctx, r)
	if err != nil {
		return
	}
	r.ID = id
	if d.Action == ActionAskModel && s.cfg.ModelAssisted {
		s.runModel(ctx, &r, i, runs, runtime)
		return
	}
	if d.Automatic && s.hooks.Apply != nil {
		outcome, aerr := s.hooks.Apply(ctx, d.Action, i)
		if aerr != nil {
			r.State = "failed"
			r.ActionOutcome = aerr.Error()
		} else {
			r.State = "completed"
			r.ActionOutcome = outcome
		}
	} else if d.ApprovalRequired {
		r.State = "approval_required"
		r.ActionOutcome = "park/escalation requires operator review"
	} else {
		r.ActionOutcome = "automatic actions disabled; recommendation recorded"
	}
	_ = s.db.FinishSupervisorRun(ctx, r)
	s.publish(r)
}

func (s *Service) runModel(ctx context.Context, r *store.SupervisorRun, i Incident, runs []store.StageRun, runtime Runtime) {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	if s.model == nil {
		r.State = "unavailable"
		r.ActionOutcome = "model backend unavailable"
		_ = s.db.FinishSupervisorRun(ctx, *r)
		s.publish(*r)
		return
	}
	info := s.model.Info()
	r.Model, r.Backend = info.Model, info.Backend
	if !info.ProviderReportsCost && !info.EstimateAvailable {
		r.State = "budget_blocked"
		r.ActionOutcome = "model call has neither provider cost nor configured estimate"
		_ = s.db.FinishSupervisorRun(ctx, *r)
		s.publish(*r)
		return
	}
	if ok, reason := s.modelBudgetAllows(ctx, i.BatchID, &i.BookID); !ok {
		r.State = "budget_blocked"
		r.ActionOutcome = reason
		_ = s.db.FinishSupervisorRun(ctx, *r)
		s.publish(*r)
		return
	}
	bounded := s.modelContext(ctx, i, runs, runtime)
	var total agent.Usage
	var decision ModelDecision
	var err error
	providerSeen, providerComplete := false, true
	estimateSeen, estimateComplete := false, true
	maxCalls := s.cfg.MaxModelCalls
	usedCalls, countErr := s.db.SupervisorInvocationCountSince(ctx, time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if countErr != nil {
		r.State = "failed"
		r.ActionOutcome = countErr.Error()
		_ = s.db.FinishSupervisorRun(ctx, *r)
		s.publish(*r)
		return
	}
	if remaining := s.cfg.InvocationsPerHour - usedCalls; remaining < maxCalls {
		maxCalls = remaining
	}
	priorBookSpend, _, _ := s.db.SupervisorSpend(ctx, i.BatchID, &i.BookID)
	priorBatchSpend, _, _ := s.db.SupervisorSpend(ctx, i.BatchID, nil)
	modelCtx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	for call := 0; call < maxCalls; call++ {
		var u agent.Usage
		r.ModelCalls++
		decision, u, err = s.model.Diagnose(modelCtx, bounded)
		reported := u.CostReported || u.CostUSD > 0
		providerSeen = providerSeen || reported
		providerComplete = providerComplete && reported
		measurable := u.Input+u.Output+u.CacheRead > 0
		_, priced := s.pricing.Estimate(info.Backend, info.Model, u.Input, u.Output, u.CacheRead)
		estimateSeen = estimateSeen || (measurable && priced)
		estimateComplete = estimateComplete && measurable && priced
		accumulateUsage(&total, u, r.ModelCalls == 1)
		if err == nil {
			break
		}
		if call+1 < maxCalls {
			spent, known := modelUsageCost(total, info, s.pricing)
			if !known || (s.cfg.PerBookBudgetUSD > 0 && priorBookSpend+spent >= s.cfg.PerBookBudgetUSD) ||
				(s.cfg.OverallBatchBudgetUSD > 0 && priorBatchSpend+spent >= s.cfg.OverallBatchBudgetUSD) {
				err = fmt.Errorf("%w; additional supervisor retry blocked by cost budget", err)
				break
			}
		}
	}
	r.InputTokens, r.OutputTokens, r.CachedTokens = total.Input, total.Output, total.CacheRead
	if providerSeen {
		v := total.CostUSD
		r.ProviderCostUSD = &v
	}
	r.ProviderCostComplete = r.ModelCalls > 0 && providerComplete
	if v, ok := s.pricing.Estimate(info.Backend, info.Model, total.Input, total.Output, total.CacheRead); ok && estimateSeen {
		r.EstimatedAPICostUSD = &v
	}
	r.EstimateComplete = r.ModelCalls > 0 && estimateComplete
	if err != nil {
		r.State = "failed"
		r.ActionOutcome = err.Error()
		_ = s.db.FinishSupervisorRun(ctx, *r)
		s.publish(*r)
		return
	}
	r.Diagnosis = decision.Diagnosis
	r.Confidence = decision.Confidence
	e, _ := json.Marshal(decision.Evidence)
	r.Evidence = e
	r.Decision = "model_assisted"
	r.SelectedAction = string(decision.RecommendedAction)
	r.SuggestedRetryLimit = decision.SuggestedRetryLimit
	r.SuggestedTerminationLimit = decision.SuggestedTerminationLimit
	r.ApprovalRequired = decision.HumanApprovalRequired
	if decision.RecommendedAction == ActionFallbackBackend && !s.cfg.AllowBackendFailover {
		r.ApprovalRequired = true
	}
	automatic := s.cfg.AutomaticActions && s.cfg.ModelAutomaticActions && !decision.HumanApprovalRequired && decision.RecommendedAction != ActionFallbackBackend
	if decision.RecommendedAction == ActionFallbackBackend && s.cfg.AllowBackendFailover {
		automatic = s.cfg.AutomaticActions && s.cfg.ModelAutomaticActions && !r.ApprovalRequired
	}
	r.Automatic = automatic
	if automatic && s.hooks.Apply != nil {
		outcome, aerr := s.hooks.Apply(ctx, decision.RecommendedAction, i)
		if aerr != nil {
			r.State = "failed"
			r.ActionOutcome = aerr.Error()
		} else {
			r.State = "completed"
			r.ActionOutcome = outcome
		}
	} else if r.ApprovalRequired {
		r.State = "approval_required"
		r.ActionOutcome = "model recommendation requires operator review"
	} else {
		r.State = "recommended"
		r.ActionOutcome = "model automatic actions disabled"
	}
	_ = s.db.FinishSupervisorRun(ctx, *r)
	s.publish(*r)
}

func modelUsageCost(u agent.Usage, info ModelInfo, prices pricing.Table) (float64, bool) {
	if u.CostReported || u.CostUSD > 0 {
		return u.CostUSD, true
	}
	return prices.Estimate(info.Backend, info.Model, u.Input, u.Output, u.CacheRead)
}

func accumulateUsage(dst *agent.Usage, u agent.Usage, first bool) {
	dst.Input += u.Input
	dst.Output += u.Output
	dst.CacheRead += u.CacheRead
	dst.CostUSD += u.CostUSD
	reported := u.CostReported || u.CostUSD > 0
	if first {
		dst.CostReported = reported
	} else {
		dst.CostReported = dst.CostReported && reported
	}
	dst.Turns += u.Turns
	if u.Model != "" {
		dst.Model = u.Model
	}
}

func (s *Service) modelBudgetAllows(ctx context.Context, batch string, book *int64) (bool, string) {
	count, err := s.db.SupervisorInvocationCountSince(ctx, time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if err != nil {
		return false, err.Error()
	}
	if count >= s.cfg.InvocationsPerHour {
		return false, "supervisor invocation-per-hour limit reached"
	}
	bookCost, unknown, err := s.db.SupervisorSpend(ctx, batch, book)
	if err != nil {
		return false, err.Error()
	}
	if unknown > 0 {
		return false, "prior supervisor calls have unknown cost"
	}
	if s.cfg.PerBookBudgetUSD > 0 && bookCost >= s.cfg.PerBookBudgetUSD {
		return false, "per-book supervisor budget reached"
	}
	batchCost, unknown, err := s.db.SupervisorSpend(ctx, batch, nil)
	if err != nil {
		return false, err.Error()
	}
	if unknown > 0 {
		return false, "prior batch supervisor calls have unknown cost"
	}
	if s.cfg.OverallBatchBudgetUSD > 0 && batchCost >= s.cfg.OverallBatchBudgetUSD {
		return false, "overall batch supervisor budget reached"
	}
	return true, ""
}

func (s *Service) modelContext(ctx context.Context, i Incident, runs []store.StageRun, runtime Runtime) ModelContext {
	b, _ := s.db.GetBook(ctx, i.BookID)
	var c ModelContext
	c.Incident = i
	c.Book.ID = b.ID
	c.Book.BatchID = b.BatchID
	c.Book.Title = truncate(b.Title, 160)
	c.Book.State = b.State
	c.Book.Status = b.Status
	c.Book.ParkCode = b.ParkCode
	c.Book.Error = truncate(b.Error, 500)
	start := 0
	if len(runs) > 8 {
		start = len(runs) - 8
	}
	for _, r := range runs[start:] {
		metrics := r.Metrics
		if len(metrics) > 1200 {
			metrics = json.RawMessage(`{"truncated":true}`)
		}
		c.Attempts = append(c.Attempts, AttemptContext{Stage: r.Stage, Attempt: r.Attempt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, OK: r.Ok, HeartbeatAt: r.HeartbeatAt, ProgressAt: r.ProgressAt, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CachedTokens: r.CacheReadTokens, ReportedCostUSD: r.CostUSD, Metrics: metrics})
	}
	c.Scheduler = SchedulerContext{AgentActive: runtime.AgentActive, AgentCapacity: runtime.AgentCapacity, EligibleAgentBooks: runtime.EligibleAgentBooks}
	events, _ := s.db.ListEvents(ctx, b.ID, 0, 8)
	for _, e := range events {
		p := e.Payload
		if len(p) > 600 {
			p = json.RawMessage(`{"truncated":true}`)
		}
		c.LogTail = append(c.LogTail, LogContext{TS: e.TS, Type: e.Type, Payload: p})
	}
	return c
}

func (s *Service) Ask(ctx context.Context, bookID int64) (store.SupervisorRun, error) {
	if !s.cfg.ModelAssisted {
		return store.SupervisorRun{}, ErrModelDisabled
	}
	b, err := s.db.GetBook(ctx, bookID)
	if err != nil {
		return store.SupervisorRun{}, err
	}
	runs, err := s.db.ListStageRuns(ctx, bookID)
	if err != nil {
		return store.SupervisorRun{}, err
	}
	runtime := Runtime{ActiveBooks: map[int64]bool{}}
	if s.hooks.Runtime != nil {
		runtime = s.hooks.Runtime()
	}
	i := Incident{Kind: IncidentUnclassified, BookID: b.ID, BatchID: b.BatchID, Stage: b.State, Diagnosis: "manual supervisor request", Evidence: []string{"operator requested bounded diagnosis"}, Ambiguous: true}
	e, _ := json.Marshal(i.Evidence)
	bid := b.ID
	r := store.SupervisorRun{BatchID: b.BatchID, BookID: &bid, Trigger: "manual_ask", Diagnosis: i.Diagnosis, Confidence: 0, Evidence: e, Decision: "pending_model", SelectedAction: string(ActionAskModel), State: "open", PricingVersion: s.pricing.Version}
	id, err := s.db.StartSupervisorRun(ctx, r)
	if err != nil {
		return r, err
	}
	r.ID = id
	s.runModel(ctx, &r, i, runs, runtime)
	recent, err := s.db.RecentSupervisorRuns(ctx, b.BatchID, 20)
	if err == nil {
		for _, x := range recent {
			if x.ID == id {
				return x, nil
			}
		}
	}
	return r, err
}

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := "monitoring"
	if !s.cfg.Enabled {
		st = "disabled"
	}
	runtime := Runtime{ActiveBooks: map[int64]bool{}}
	if s.hooks.Runtime != nil {
		runtime = s.hooks.Runtime()
	}
	out := Status{State: st, Enabled: s.cfg.Enabled, AutomaticActions: s.cfg.AutomaticActions, ModelAssisted: s.cfg.ModelAssisted, ModelAvailable: s.model != nil, AllowBackendFailover: s.cfg.AllowBackendFailover, Runtime: runtime, LastError: s.lastErr}
	if !s.lastCheck.IsZero() {
		out.LastCheckAt = s.lastCheck.Format(time.RFC3339Nano)
	}
	return out
}
func (s *Service) Recent(ctx context.Context, batch string, limit int) ([]store.SupervisorRun, error) {
	return s.db.RecentSupervisorRuns(ctx, batch, limit)
}
func (s *Service) Costs(ctx context.Context, batch string) (store.BatchCostSummary, error) {
	return s.db.BatchCosts(ctx, batch)
}
func (s *Service) publish(r store.SupervisorRun) {
	if s.hooks.Publish != nil {
		bookID := int64(0)
		if r.BookID != nil {
			bookID = *r.BookID
		}
		s.hooks.Publish("supervisor.decision", bookID, r)
	}
}

func incidentKey(i Incident) string {
	return fmt.Sprintf("%s/%d/%s/%d/%s", i.Kind, i.BookID, i.Stage, i.StageRunID, i.Fingerprint)
}

func artifactStatuses(book store.Book, runs []store.StageRun) []ArtifactStatus {
	var out []ArtifactStatus
	seen := map[string]bool{}
	for _, r := range runs {
		if r.Ok == nil || !*r.Ok || r.Superseded || seen[r.Stage] {
			continue
		}
		if book.State == "done" && r.Stage == "splitting" {
			continue // done-book scratch purge intentionally invalidates the splitting sentinel
		}
		seen[r.Stage] = true
		path := filepath.Join(book.WorkDir, "_done", r.Stage+".json")
		raw, err := os.ReadFile(path) //nolint:gosec // path is constrained to the stored book work dir and known sentinel name
		var sentinel struct {
			Stage string `json:"stage"`
			Runs  int    `json:"runs"`
			At    string `json:"at"`
		}
		decodeErr := json.Unmarshal(raw, &sentinel)
		valid := err == nil && decodeErr == nil && sentinel.Stage == r.Stage && sentinel.Runs > 0 && sentinel.At != ""
		reason := ""
		if err != nil {
			reason = err.Error()
		} else if !valid {
			reason = "sentinel JSON is invalid or its stage/runs/timestamp fields do not match"
		}
		out = append(out, ArtifactStatus{Stage: r.Stage, StageRunID: r.ID, Path: path, Valid: valid, Reason: reason})
		for _, rel := range requiredArtifacts[r.Stage] {
			p := filepath.Join(book.WorkDir, rel)
			artifact, e := os.ReadFile(p) //nolint:gosec // rel comes from the compiled required-artifact allow-list
			ok := e == nil && len(artifact) > 0 && json.Valid(artifact)
			why := ""
			if e != nil {
				why = e.Error()
			} else if !ok {
				why = "artifact is empty or invalid JSON"
			}
			out = append(out, ArtifactStatus{Stage: r.Stage, StageRunID: r.ID, Path: p, Valid: ok, Reason: why})
		}
	}
	return out
}

var requiredArtifacts = map[string][]string{"inspecting": {"manifest.json"}, "asr": {"asr.json"}, "qa_sweep": {"qa_report.json"}, "validating": {"validation_report.json"}, "auditing": {"audit.json"}}
