// Package state is the per-book pipeline state machine: a pure, table-driven
// description of the extraction pipeline's stages, the lanes they run in, and the
// legal transitions between them. It holds NO I/O and no scheduler concerns - the
// scheduler (internal/scheduler) and the store (internal/store) consume it. Being
// dependency-free keeps every rule exhaustively unit-testable.
//
// There are two front halves, selected by the book's Kind, feeding one shared
// authoring tail.
//
// Audio mirrors EXTRACTION-AUDIO.md (validated on 11+ books):
//
//	queued -> inspecting -> [markers_normalizing] -> splitting -> asr -> sanitizing
//	-> qa_sweep -> [qa_adjudicating] -> [retranscribing -> qa_sweep]loop
//	-> spelling_research -> correcting -> fact_pass -> ...
//
// Ebook mirrors EXTRACTION.md, whose whole point is that exact text needs none of
// the machinery that reconstructs it from audio:
//
//	queued -> extracting -> [chapter_mapping] -> fact_pass -> ...
//
// and both then run the shared tail:
//
//	... -> synthesizing -> validating -> auditing -> [fixing -> validating]loop(max 3)
//	-> ready -> contributing -> done
//
// States in [brackets] are conditional (they may be skipped) or loop back.
package state

import "fmt"

// Kind is a book's source modality: which front half of the pipeline it runs.
// It is durable (books.kind) and chosen when the book is enqueued.
type Kind string

const (
	// KindAudio is the default and what every pre-migration row is: an audiobook
	// folder, transcribed and corrected before the authoring tail.
	KindAudio Kind = "audio"
	// KindEbook is a DRM-free epub, whose text is exact, so the audio front half
	// (inspect/split/ASR/sanitize/QA/spelling) is skipped entirely.
	KindEbook Kind = "ebook"
)

// ParseKind normalizes a stored kind, mapping "" to KindAudio. Lenient by design:
// it is the DB read path, and rows written before the kind column existed carry
// the empty string. Use ValidKind at an input boundary, where a typo must be
// rejected rather than silently treated as audio.
func ParseKind(s string) Kind {
	if Kind(s) == KindEbook {
		return KindEbook
	}
	return KindAudio
}

// ValidKind reports whether s names a kind, accepting "" as the audio default.
func ValidKind(s string) bool {
	switch Kind(s) {
	case "", KindAudio, KindEbook:
		return true
	default:
		return false
	}
}

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
	Extracting         State = "extracting"
	ChapterMapping     State = "chapter_mapping"
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
// The ebook states sit at order 11/12, immediately before FactPass, rather than
// at the front beside Inspecting. Two reasons, both load-bearing:
//
//   - order is compared, not just ordered: scheduler.queueGroup buckets anything
//     with Order(st) <= Order(ASR) as ASR work, so an ebook in Extracting would
//     render in the Running tab's ASR section despite never touching that lane.
//   - order reads as "distance from Ready" for BOTH kinds this way: an ebook in
//     Extracting genuinely is one step from FactPass, exactly like an audio book
//     in Correcting.
//
// Nothing persists order (books.state stores the state string), so placing them
// here costs only this comment.
var table = map[State]Def{
	Queued:             {Lane: LaneNone, Next: []State{Extracting, Inspecting}, order: 0},
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
	Extracting:         {Lane: LaneMechanical, Next: []State{ChapterMapping, FactPass}, order: 11},
	ChapterMapping:     {Lane: LaneAgent, Next: []State{FactPass}, Agent: true, order: 12},
	FactPass:           {Lane: LaneAgent, Next: []State{Synthesizing}, Agent: true, order: 13},
	Synthesizing:       {Lane: LaneAgent, Next: []State{Validating}, Agent: true, order: 14},
	Validating:         {Lane: LaneMechanical, Next: []State{Auditing}, order: 15},
	Auditing:           {Lane: LaneAgent, Next: []State{Fixing, Ready}, Agent: true, order: 16},
	Fixing:             {Lane: LaneAgent, Next: []State{Validating}, Agent: true, order: 17},
	Ready:              {Lane: LaneNone, Next: []State{Contributing}, order: 18},
	Contributing:       {Lane: LaneMechanical, Next: []State{Done}, order: 19},
	Done:               {Lane: LaneNone, Next: nil, Terminal: true, order: 20},
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
// Queued is the one fork the table cannot express, because BOTH successors are a
// mainline - Extracting for an ebook, Inspecting for an audio book - so the
// "mainline continuation last" convention has nothing to say. kind decides it
// here; every other state's mainline is kind-independent, since the two front
// halves converge on FactPass.
func MainlineNext(kind Kind, s State) State {
	if s == Queued {
		if kind == KindEbook {
			return Extracting
		}
		return Inspecting
	}
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

// RequiresSeriesOrder reports whether an agent stage consumes series carryover or
// authors/validates spoiler-sensitive sidecars. Marker normalization and QA
// adjudication operate only on the current book's audio/transcript, so they may run
// while an earlier book in the same series is parked. The authoring tail must still
// wait for the series lock holder to reach Ready.
func RequiresSeriesOrder(s State) bool {
	switch s {
	case SpellingResearch, FactPass, Synthesizing, Auditing, Fixing:
		return true
	default:
		return false
	}
}

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
	// ChaptersMapped (extracting): the epub's toc labels already yield a contiguous
	// logical chapter universe, so chapter_mapping can be skipped. It deliberately
	// does NOT reuse MarkersContiguous, whose meaning is identical: these values are
	// written into the stage sentinel JSON a human reads when debugging a parked
	// book, and "markers_contiguous" on an epub is a lie.
	ChaptersMapped bool
}

// NextState computes the forward transition from cur given a completed stage's
// Outcome. The returned Status is StatusNone except when the fix loop is
// exhausted, where it returns (Auditing, StatusNeedsAttention) to park the book.
// The returned state is always a table-declared successor of cur (asserted by
// tests) except the park case, which stays on Auditing by design.
func NextState(kind Kind, cur State, o Outcome) (State, Status, error) {
	def, ok := table[cur]
	if !ok {
		return "", StatusNone, fmt.Errorf("unknown state %q", cur)
	}
	if def.Terminal {
		return "", StatusNone, fmt.Errorf("state %q is terminal", cur)
	}

	var next State
	switch cur {
	case Queued:
		// The one place kind selects a front half. It is an explicit parameter
		// rather than an Outcome field because advanceWaypoints dispatches
		// waypoints with a ZERO Outcome: a kind field there would default to
		// audio and route every ebook into ffprobe.
		if kind == KindEbook {
			next = Extracting
		} else {
			next = Inspecting
		}
	case Extracting:
		if o.ChaptersMapped {
			next = FactPass // the toc already yields a contiguous chapter run
		} else {
			next = ChapterMapping
		}
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
	if IsAgent(cur) && RequiresSeriesOrder(cur) && !lowestInSeries {
		return false
	}
	return true
}
