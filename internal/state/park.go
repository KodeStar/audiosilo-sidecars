package state

// ParkCode is a machine-readable reason a book was parked needs_attention,
// carried beside the free-text error message so a client can render a per-class
// affordance hint (retry now, install a tool, delete and re-enqueue, ...). Empty
// means "no code" - a status that is not a park, or a park with no typed reason.
//
// Pure data: the scheduler sets it from a ParkError's Code, the state-machine
// fix-loop cap sets ParkFixLoopExhausted directly, and the store persists it in
// books.park_code (cleared whenever the status clears).
type ParkCode string

// The typed park reasons. Each corresponds to one class of pipeline park site
// (internal/pipeline), except ParkFixLoopExhausted, which the scheduler sets when
// the audit->fix loop is exhausted (state.NextState returns StatusNeedsAttention).
const (
	ParkAgentUnavailable         ParkCode = "agent_unavailable"
	ParkAgentRateLimited         ParkCode = "agent_rate_limited"
	ParkAgentValidationExhausted ParkCode = "agent_validation_exhausted"
	ParkMarkersNotConfident      ParkCode = "markers_not_confident"
	ParkQANoConverge             ParkCode = "qa_no_converge"
	ParkSpellingGateFailure      ParkCode = "spelling_gate_failure"
	ParkMediaToolsUnavailable    ParkCode = "media_tools_unavailable"
	ParkASRUnavailable           ParkCode = "asr_unavailable"
	ParkManifestChanged          ParkCode = "manifest_changed"
	ParkFixLoopExhausted         ParkCode = "fix_loop_exhausted"
)
