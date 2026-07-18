// Package state is the per-book pipeline state machine: a pure, table-driven
// description of the extraction pipeline's stages, the lanes they run in, and the
// legal transitions between them. It holds NO I/O and no scheduler concerns - the
// scheduler (internal/scheduler) and the store (internal/store) consume it. Being
// dependency-free keeps every rule exhaustively unit-testable.
//
// The pipeline mirrors EXTRACTION-AUDIO.md (validated on 11+ books):
//
//	queued -> inspecting -> [markers_normalizing] -> splitting -> asr -> sanitizing
//	-> qa_sweep -> [qa_adjudicating] -> [retranscribing -> qa_sweep]loop
//	-> spelling_research -> correcting -> fact_pass -> synthesizing
//	-> validating -> auditing -> [fixing -> validating]loop(max 3)
//	-> ready -> contributing -> done
//
// States in [brackets] are conditional (they may be skipped) or loop back.
package state

import "fmt"

// State is a node in the pipeline. queued/ready/done are waypoints with no lane;
// every other state is a "stage" that a lane worker executes.
type State string

// The pipeline states, in canonical forward order.
const (
	Queued             State = "queued"
	Inspecting         State = "inspecting"
	MarkersNormalizing State = "markers_normalizing"
	Splitting          State = "splitting"
	ASR                State = "asr"
	Sanitizing         State = "sanitizing"
	QASweep            State = "qa_sweep"
	QAAdjudicating     State = "qa_adjudicating"
	Retranscribing     State = "retranscribing"
	SpellingResearch   State = "spelling_research"
	Correcting         State = "correcting"
	FactPass           State = "fact_pass"
	Synthesizing       State = "synthesizing"
	Validating         State = "validating"
	Auditing           State = "auditing"
	Fixing             State = "fixing"
	Ready              State = "ready"
	Contributing       State = "contributing"
	Done               State = "done"
)

// Lane is the executor pool a stage runs in. The three lanes have different
// resource profiles (GPU-bound ASR, rate-limited agents, cheap mechanical work)
// and independent capacities, so they run concurrently.
type Lane string

// The lanes. LaneNone marks a waypoint (queued/ready/done) that runs no executor.
const (
	LaneNone       Lane = ""
	LaneASR        Lane = "asr"
	LaneAgent      Lane = "agent"
	LaneMechanical Lane = "mechanical"
)

// Status is an orthogonal flag layered over the State. It records an exceptional
// condition (paused/parked/failed) without losing the underlying pipeline
// position, so clearing it resumes exactly where the book left off.
type Status string

// The statuses. StatusNone is the normal running condition.
const (
	StatusNone           Status = ""
	StatusPaused         Status = "paused"
	StatusNeedsAttention Status = "needs_attention"
	StatusFailed         Status = "failed"
)

// MaxFixAttempts caps the audit->fix->re-validate loop. After this many fix
// passes still fail the audit, the book is parked needs_attention for a human.
const MaxFixAttempts = 3

// Def is the static table entry for a State: its lane, the exhaustive set of
// legal next states, and classification flags. NextState always returns a member
// of Next (or an error), so the table doubles as the transition contract that
// tests assert against.
type Def struct {
	Lane     Lane
	Next     []State
	Agent    bool // runs in an agent (LLM) lane
	Terminal bool // no outgoing transitions
	order    int  // canonical linear position, for reconcile ordering
}

// table is the single source of truth for the state machine. order is the
// canonical linear index used to compare "how far" two stages are (loops reuse
// the index of the stage they re-enter conceptually, but each state has a unique
// index here so ordering is total).
//
// Conditional states (may be skipped by a branch, or loop back) are the ones
// shown in [brackets] in the package doc: markers_normalizing, qa_adjudicating,
// retranscribing, and fixing. The skip/loop routing is encoded directly in
// NextState's branch rules, so the classification needs no table column.
//
// Next ORDERING CONVENTION: when a state has more than one successor, the
// conditional/loop target is listed FIRST and the mainline (happy-branch)
// continuation LAST. MainlineNext depends on this ordering (it returns the last
// successor), so keep the mainline entry last when editing a multi-successor row.
var table = map[State]Def{
	Queued:             {Lane: LaneNone, Next: []State{Inspecting}, order: 0},
	Inspecting:         {Lane: LaneMechanical, Next: []State{MarkersNormalizing, Splitting}, order: 1},
	MarkersNormalizing: {Lane: LaneAgent, Next: []State{Splitting}, Agent: true, order: 2},
	Splitting:          {Lane: LaneMechanical, Next: []State{ASR}, order: 3},
	ASR:                {Lane: LaneASR, Next: []State{Sanitizing}, order: 4},
	Sanitizing:         {Lane: LaneMechanical, Next: []State{QASweep}, order: 5},
	QASweep:            {Lane: LaneMechanical, Next: []State{QAAdjudicating, SpellingResearch}, order: 6},
	QAAdjudicating:     {Lane: LaneAgent, Next: []State{Retranscribing, SpellingResearch}, Agent: true, order: 7},
	Retranscribing:     {Lane: LaneASR, Next: []State{QASweep}, order: 8},
	SpellingResearch:   {Lane: LaneAgent, Next: []State{Correcting}, Agent: true, order: 9},
	Correcting:         {Lane: LaneMechanical, Next: []State{FactPass}, order: 10},
	FactPass:           {Lane: LaneAgent, Next: []State{Synthesizing}, Agent: true, order: 11},
	Synthesizing:       {Lane: LaneAgent, Next: []State{Validating}, Agent: true, order: 12},
	Validating:         {Lane: LaneMechanical, Next: []State{Auditing}, order: 13},
	Auditing:           {Lane: LaneAgent, Next: []State{Fixing, Ready}, Agent: true, order: 14},
	Fixing:             {Lane: LaneAgent, Next: []State{Validating}, Agent: true, order: 15},
	Ready:              {Lane: LaneNone, Next: []State{Contributing}, order: 16},
	Contributing:       {Lane: LaneMechanical, Next: []State{Done}, order: 17},
	Done:               {Lane: LaneNone, Next: nil, Terminal: true, order: 18},
}

// All returns every state in canonical forward order.
func All() []State {
	out := make([]State, 0, len(table))
	byOrder := make([]State, len(table))
	for s, d := range table {
		byOrder[d.order] = s
	}
	out = append(out, byOrder...)
	return out
}

// LaneOf returns the lane a state runs in (LaneNone for waypoints).
func LaneOf(s State) Lane { return table[s].Lane }

// IsStage reports whether s is executed by a lane worker (has a real lane).
func IsStage(s State) bool { return table[s].Lane != LaneNone }

// IsAgent reports whether s runs in the agent lane.
func IsAgent(s State) bool { return table[s].Agent }

// SupportsAgentFanout reports the stages with a proven isolated fragment/merge
// contract. Other agent stages remain serial for whole-book consistency.
func SupportsAgentFanout(s State) bool { return s == FactPass || s == QAAdjudicating }

// IsTerminal reports whether s has no outgoing transitions (Done).
func IsTerminal(s State) bool { return table[s].Terminal }

// IsWaypoint reports whether s is a non-terminal state with no lane, which the
// scheduler advances immediately without running an executor (queued, ready).
func IsWaypoint(s State) bool {
	d := table[s]
	return d.Lane == LaneNone && !d.Terminal
}

// Order returns the canonical linear index of s (for reconcile ordering).
func Order(s State) int { return table[s].order }

// MainlineNext returns s's happy-branch successor: the mainline continuation that
// the pipeline follows when every conditional is skipped and no loop is taken. By
// the table's Next ORDERING CONVENTION (conditional/loop target first, mainline
// continuation last) that is always the LAST declared successor, so following
// MainlineNext from Queued walks the pipeline's mainline to Done, skipping the
// bracketed conditional stages. It returns "" for a terminal state (no successors).
// It is the derivation the ETA engine's optimistic path uses.
func MainlineNext(s State) State {
	next := table[s].Next
	if len(next) == 0 {
		return ""
	}
	return next[len(next)-1]
}

// HoldsSeriesLock reports whether a book at state s still holds its series lock:
// true for every state before Ready. A book that has reached Ready (or beyond)
// has finished authoring, so it no longer blocks its series' successors from the
// agent lane. The scheduler uses this to pick each series' lock holder.
func HoldsSeriesLock(s State) bool { return Order(s) < Order(Ready) }

// legalNext reports whether next is a declared successor of cur.
func legalNext(cur, next State) bool {
	for _, n := range table[cur].Next {
		if n == next {
			return true
		}
	}
	return false
}

// Outcome carries the branch decisions and counters a completed stage feeds into
// NextState. Only the fields relevant to cur's branch are consulted; the rest are
// ignored, so an executor may zero-fill.
type Outcome struct {
	// MarkersContiguous (inspecting): markers already line up, so
	// markers_normalizing can be skipped.
	MarkersContiguous bool
	// QAClean (qa_sweep): no degeneration found, so adjudication is skipped.
	QAClean bool
	// RetranscribeNeeded (qa_adjudicating): a flagged chapter must be redone.
	RetranscribeNeeded bool
	// AuditPassed (auditing): the adversarial audit passed; go to ready.
	AuditPassed bool
	// FixAttempts (auditing): fix passes already completed, for the cap.
	FixAttempts int
}

// NextState computes the forward transition from cur given a completed stage's
// Outcome. The returned Status is StatusNone except when the fix loop is
// exhausted, where it returns (Auditing, StatusNeedsAttention) to park the book.
// The returned state is always a table-declared successor of cur (asserted by
// tests) except the park case, which stays on Auditing by design.
func NextState(cur State, o Outcome) (State, Status, error) {
	def, ok := table[cur]
	if !ok {
		return "", StatusNone, fmt.Errorf("unknown state %q", cur)
	}
	if def.Terminal {
		return "", StatusNone, fmt.Errorf("state %q is terminal", cur)
	}

	var next State
	switch cur {
	case Inspecting:
		if o.MarkersContiguous {
			next = Splitting // skip markers_normalizing
		} else {
			next = MarkersNormalizing
		}
	case QASweep:
		if o.QAClean {
			next = SpellingResearch // skip adjudication
		} else {
			next = QAAdjudicating
		}
	case QAAdjudicating:
		if o.RetranscribeNeeded {
			next = Retranscribing
		} else {
			next = SpellingResearch
		}
	case Auditing:
		switch {
		case o.AuditPassed:
			next = Ready
		case o.FixAttempts >= MaxFixAttempts:
			// Fix budget spent and the audit still fails: park for a human.
			return Auditing, StatusNeedsAttention, nil
		default:
			next = Fixing
		}
	default:
		// Deterministic single-successor states.
		if len(def.Next) != 1 {
			return "", StatusNone, fmt.Errorf("state %q needs an explicit branch rule", cur)
		}
		next = def.Next[0]
	}

	if !legalNext(cur, next) {
		return "", StatusNone, fmt.Errorf("illegal transition %q -> %q", cur, next)
	}
	return next, StatusNone, nil
}

// CanStart reports whether a book at state cur/status may be dispatched to its
// lane now. It is a pure guard: the scheduler supplies the series-lock verdict
// (lowestInSeries), since only the lowest-position unfinished book in a series
// may hold an agent slot. Non-agent stages ignore the series lock. A book that is
// paused/parked/failed, terminal, or a waypoint is not directly startable (a
// waypoint is auto-advanced by the scheduler, not lane-dispatched).
func CanStart(cur State, status Status, lowestInSeries bool) bool {
	if status != StatusNone {
		return false
	}
	if !IsStage(cur) {
		return false
	}
	if IsAgent(cur) && !lowestInSeries {
		return false
	}
	return true
}
