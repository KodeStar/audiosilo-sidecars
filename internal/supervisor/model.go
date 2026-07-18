package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/pricing"
)

// ModelContext is the complete bounded payload available to a model supervisor.
// Log tails and histories are capped by the service before this value is built.
type ModelContext struct {
	Incident Incident `json:"incident"`
	Book     struct {
		ID       int64  `json:"id"`
		BatchID  string `json:"batch_id"`
		Title    string `json:"title"`
		State    string `json:"state"`
		Status   string `json:"status"`
		ParkCode string `json:"park_code,omitempty"`
		Error    string `json:"error,omitempty"`
	} `json:"book"`
	Attempts  []AttemptContext `json:"attempts"`
	Scheduler SchedulerContext `json:"scheduler"`
	LogTail   []LogContext     `json:"log_tail"`
}

type AttemptContext struct {
	Stage           string          `json:"stage"`
	Attempt         int             `json:"attempt"`
	StartedAt       string          `json:"started_at"`
	FinishedAt      string          `json:"finished_at,omitempty"`
	OK              *bool           `json:"ok"`
	HeartbeatAt     string          `json:"heartbeat_at,omitempty"`
	ProgressAt      string          `json:"progress_at,omitempty"`
	InputTokens     int64           `json:"input_tokens"`
	OutputTokens    int64           `json:"output_tokens"`
	CachedTokens    int64           `json:"cached_tokens"`
	ReportedCostUSD float64         `json:"reported_cost_usd"`
	Metrics         json.RawMessage `json:"metrics"`
}

type SchedulerContext struct {
	AgentActive        int `json:"agent_active"`
	AgentCapacity      int `json:"agent_capacity"`
	EligibleAgentBooks int `json:"eligible_agent_books"`
}

type LogContext struct {
	TS      string          `json:"ts"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type ModelInfo struct {
	Backend             string
	Model               string
	ProviderReportsCost bool
	EstimateAvailable   bool
}

type Model interface {
	Info() ModelInfo
	Diagnose(context.Context, ModelContext) (ModelDecision, agent.Usage, error)
}

type AgentModel struct {
	runner   agent.Runner
	backend  string
	model    string
	dir      string
	timeout  time.Duration
	maxTurns int
	pricing  pricing.Table
}

func NewAgentModel(runner agent.Runner, model, dir string, timeout time.Duration, maxTurns int, prices pricing.Table) *AgentModel {
	backend := ""
	if runner != nil {
		backend = runner.ID()
	}
	return &AgentModel{runner: runner, backend: backend, model: model, dir: dir, timeout: timeout, maxTurns: maxTurns, pricing: prices}
}

func (m *AgentModel) Info() ModelInfo {
	_, estimate := m.pricing.Estimate(m.backend, m.model, 1, 1, 1)
	return ModelInfo{Backend: m.backend, Model: m.model, ProviderReportsCost: m.backend == agent.IDClaude, EstimateAvailable: estimate}
}

func (m *AgentModel) Diagnose(ctx context.Context, bounded ModelContext) (ModelDecision, agent.Usage, error) {
	if m.runner == nil {
		return ModelDecision{}, agent.Usage{}, errors.New("model supervisor backend unavailable")
	}
	payload, err := json.Marshal(bounded)
	if err != nil {
		return ModelDecision{}, agent.Usage{}, err
	}
	prompt := `You are a bounded batch health supervisor, not a coding or content agent.
Use only the JSON context below. Do not inspect files, edit code or prompts, change facts/documents,
increase budgets, change global backends, or publish outputs. Return exactly one JSON object with:
diagnosis (string), confidence (0..1), evidence (string array), recommended_action (one of observe,
retry, readmit, requeue, terminate_requeue, supersede_rerun, stop_budget, reallocate,
fallback_backend, park_escalate), human_approval_required (boolean), suggested_retry_limit
(non-negative integer), suggested_termination_limit (non-negative integer).
Context:
` + string(payload)
	res, err := m.runner.Run(ctx, agent.Request{Stage: "supervisor", Dir: m.dir, Prompt: prompt, Model: m.model,
		Timeout: m.timeout, MaxTurns: m.maxTurns, NoTools: true})
	if err != nil {
		return ModelDecision{}, res.Usage, err
	}
	var d ModelDecision
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(res.Text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return ModelDecision{}, res.Usage, fmt.Errorf("model supervisor output: %w", err)
	}
	if d.Diagnosis == "" || d.Confidence < 0 || d.Confidence > 1 || !IsAllowedAction(d.RecommendedAction) || d.RecommendedAction == ActionAskModel || d.SuggestedRetryLimit < 0 || d.SuggestedTerminationLimit < 0 {
		return ModelDecision{}, res.Usage, errors.New("model supervisor output failed schema constraints")
	}
	return d, res.Usage, nil
}
