package state

import "slices"

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

	// ParkASRDecodeTimeout: a chapter's whisper decode exceeded its per-chapter time bound
	// even after a NoContext retry (a repetition-collapse decode running many times the audio
	// length). Human-only: inspect the chapter audio, then Retry. Not auto-readmitted (an
	// immediate retry would just time out again).
	ParkASRDecodeTimeout ParkCode = "asr_decode_timeout"

	// ParkContribUnavailable: the contributing stage runs in issue/pr mode but no
	// GitHub credential is available (no PAT in secrets, no `gh auth token`). The user
	// adds a PAT in Settings or runs `gh auth login`, then Retry.
	ParkContribUnavailable ParkCode = "contrib_unavailable"
	// ParkCoreNeeded: the book's work does not exist upstream, so an add-work (core)
	// proposal awaits completion/confirmation in the UI before the sidecars can attach.
	ParkCoreNeeded ParkCode = "core_needed"
	// ParkCorePending: a core proposal has been submitted; the book waits for the intake
	// PR to merge, after which the poller resolves the real work slug and re-admits it.
	ParkCorePending ParkCode = "core_pending"

	// ParkEbookUnreadable: the .epub could not be opened or parsed - not a zip, no
	// container.xml/OPF, a spine entry missing from the archive, or DRM-wrapped.
	// Human-fixable precondition (supply a DRM-free epub, or enqueue the audiobook
	// instead), so it parks like the media-tools park and carries no auto-readmit.
	ParkEbookUnreadable ParkCode = "ebook_unreadable"
	// ParkEbookNoChapters: the split produced no numbered chapters AND no usable toc
	// labels (or one spine document holds the whole book with no anchors). Distinct
	// from ParkEbookChaptersNotConfident because there is nothing for an agent to map -
	// the file itself is the problem, so the operator hint differs.
	ParkEbookNoChapters ParkCode = "ebook_no_chapters"
	// ParkEbookChaptersNotConfident: chapter_mapping could not defensibly map the toc
	// onto a contiguous chapter universe. The direct analogue of
	// ParkMarkersNotConfident, and parked for the same reason: a guessed chapter
	// number is a wrong spoiler position, which nothing downstream can detect.
	ParkEbookChaptersNotConfident ParkCode = "ebook_chapters_not_confident"

	// ParkBudgetExceeded: the book's summed agent cost reached the configured per-book
	// budget (agent.book_budget_usd), so an agent stage parked before spending more. A
	// human decision: raise the budget in config.yaml (restart to apply), then Retry -
	// so, unlike the transient agent parks, it carries no auto-readmit time.
	ParkBudgetExceeded ParkCode = "budget_exceeded"
	// Supervisor containment parks never authorize content/code changes; a human
	// may inspect the persisted decision and explicitly Retry when satisfied.
	ParkSupervisorEscalated ParkCode = "supervisor_escalated"
	ParkSupervisorBudget    ParkCode = "supervisor_budget"
)

// SupervisorMessagePrefix labels a book.Error the supervisor rewrote on a park_escalate
// ("supervisor: <diagnosis/evidence>"). The scheduler prepends it when building the park
// message; the supervisor's parked-recovery fingerprint strips it so re-hashed escalation
// prose never leaks back into the stationary fingerprint. One const, two packages
// (internal/scheduler + internal/supervisor), instead of a duplicated literal in each.
const SupervisorMessagePrefix = "supervisor: "

// IsParkedWith reports whether a book carrying the given status/park code is parked
// (needs_attention) with a park code among want. It centralizes the
// "status == needs_attention && park code in {...}" test the api's park-gated handlers
// and contrib's re-admit path both need, keeping them tied to the park-code invariant:
// park_code is non-empty iff status is needs_attention (store-enforced), so a match on
// want implies the needs_attention status this guard also checks.
func IsParkedWith(status, code string, want ...ParkCode) bool {
	if status != string(StatusNeedsAttention) {
		return false
	}
	return slices.Contains(want, ParkCode(code))
}
