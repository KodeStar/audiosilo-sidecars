// Package supervisor implements bounded orchestration health monitoring. It does not
// execute production stages and its action vocabulary cannot edit code, prompts,
// extracted facts, generated documents, budgets, or published outputs.
package supervisor

import (
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

type IncidentKind string

const (
	IncidentStaleHeartbeat     IncidentKind = "stale_heartbeat"
	IncidentNoProgress         IncidentKind = "no_progress"
	IncidentMissingProcess     IncidentKind = "missing_process"
	IncidentRepeatedError      IncidentKind = "repeated_error"
	IncidentNonConvergingQA    IncidentKind = "qa_non_converging"
	IncidentNonConvergingAudit IncidentKind = "audit_non_converging"
	IncidentDurationLimit      IncidentKind = "duration_limit"
	IncidentTokenLimit         IncidentKind = "token_limit"
	IncidentCostLimit          IncidentKind = "cost_limit"
	IncidentAuthentication     IncidentKind = "authentication_failure"
	IncidentRateLimit          IncidentKind = "rate_limit"
	IncidentBackendUnavailable IncidentKind = "backend_unavailable"
	IncidentArtifactInvalid    IncidentKind = "artifact_invalid"
	IncidentSlotInefficiency   IncidentKind = "slot_inefficiency"
	IncidentUnclassified       IncidentKind = "unclassified"
)

type Action string

const (
	ActionObserve          Action = "observe"
	ActionRetry            Action = "retry"
	ActionReadmit          Action = "readmit"
	ActionRequeue          Action = "requeue"
	ActionTerminateRequeue Action = "terminate_requeue"
	ActionSupersedeRerun   Action = "supersede_rerun"
	ActionStopBudget       Action = "stop_budget"
	ActionReallocate       Action = "reallocate"
	ActionFallbackBackend  Action = "fallback_backend"
	ActionParkEscalate     Action = "park_escalate"
	ActionAskModel         Action = "ask_model"
)

var allowedActions = map[Action]bool{
	ActionObserve: true, ActionRetry: true, ActionReadmit: true, ActionRequeue: true,
	ActionTerminateRequeue: true, ActionSupersedeRerun: true, ActionStopBudget: true,
	ActionReallocate: true, ActionFallbackBackend: true, ActionParkEscalate: true, ActionAskModel: true,
}

func IsAllowedAction(a Action) bool { return allowedActions[a] }

type Policy struct {
	StaleAfter            time.Duration
	NoProgressAfter       time.Duration
	MaxStageDuration      time.Duration
	MaxAttempts           int
	MaxErrorRepeats       int
	MaxStageTokens        int64
	MaxStageCostUSD       float64
	AttemptGrowthFactor   float64
	AutomaticActions      bool
	ModelAssisted         bool
	ModelAutomaticActions bool
	AllowBackendFailover  bool
}

type ArtifactStatus struct {
	Stage      string
	StageRunID int64
	Path       string
	Valid      bool
	Reason     string
}

type Snapshot struct {
	Now                time.Time
	Book               store.Book
	Runs               []store.StageRun
	RuntimeActive      bool
	ProcessAlive       *bool
	Artifacts          []ArtifactStatus
	AgentActive        int
	AgentCapacity      int
	EligibleAgentBooks int
}

type Incident struct {
	Kind        IncidentKind `json:"kind"`
	BookID      int64        `json:"book_id"`
	BatchID     string       `json:"batch_id"`
	Stage       string       `json:"stage,omitempty"`
	StageRunID  int64        `json:"stage_run_id,omitempty"`
	Fingerprint string       `json:"fingerprint,omitempty"`
	Diagnosis   string       `json:"diagnosis"`
	Evidence    []string     `json:"evidence"`
	Ambiguous   bool         `json:"ambiguous"`
	Protected   bool         `json:"protected"`
}

type Decision struct {
	Incident         Incident `json:"incident"`
	Action           Action   `json:"action"`
	Automatic        bool     `json:"automatic"`
	ApprovalRequired bool     `json:"approval_required"`
	RetryLimit       int      `json:"retry_limit"`
	TerminationLimit int      `json:"termination_limit"`
}

// ModelDecision is the strict contract returned by an optional model supervisor.
type ModelDecision struct {
	Diagnosis                 string   `json:"diagnosis"`
	Confidence                float64  `json:"confidence"`
	Evidence                  []string `json:"evidence"`
	RecommendedAction         Action   `json:"recommended_action"`
	HumanApprovalRequired     bool     `json:"human_approval_required"`
	SuggestedRetryLimit       int      `json:"suggested_retry_limit"`
	SuggestedTerminationLimit int      `json:"suggested_termination_limit"`
}
