// Package eta is the pure ETA engine for the sidecars pipeline. It holds NO I/O and
// reads no clock: the scheduler injects a book list, the per-stage progress rows,
// and the current per-stage unit-rate map, and eta returns time estimates.
//
// Two estimates:
//
//   - BookETA: the seconds a single book still needs, the sum of rate*remainingUnits
//     over its remaining OPTIMISTIC path (the happy branch at every fork). It ignores
//     lane contention - it is the book's own work time, not its wall-clock finish.
//   - QueueETA: the wall-clock makespan of the whole active queue, a greedy
//     event-driven simulation over the three lanes (ASR cap 1, mechanical cap 2,
//     agent cap = configured), honouring the scheduler's ordering (retranscribe
//     jumps the ASR queue) and the series lock (only the lowest-position unfinished
//     book of a series runs agent work).
//
// Rates are EWMA-updated per stage (seconds per unit) via Observe and stored in the
// rates table; until a stage has an observed rate the in-code seed prior is used.
//
// The remaining path is OPTIMISTIC: at each fork it takes the happy branch
// (inspecting -> splitting, qa_sweep -> spelling_research, auditing -> ready). The
// conditional off-mainline stages (markers_normalizing, qa_adjudicating,
// retranscribing, fixing) are counted ONLY when a book is currently in one, then it
// routes optimistically back onto the mainline. Loops (a re-entered qa_adjudicating,
// a repeated fix pass) are NOT predicted - the estimate is a lower bound that a book
// hitting a loop will overrun, which the observed-rate feedback cannot fix (the path
// model, not the rate, is optimistic).
package eta

import (
	"math"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

// Alpha is the EWMA smoothing factor: new = Alpha*observed + (1-Alpha)*old.
const Alpha = 0.3

// Defaults used when a per-book total is not yet known.
const (
	// DefaultChapters is the assumed chapter count for the per-chapter stages when
	// books.chapters is 0 (inspect has not run) and no progress row exists.
	DefaultChapters = 60
	// DefaultChunks is the assumed fact_pass chunk count until its progress row
	// (which carries the real chunk total) exists.
	DefaultChunks = 8
)

// retranscribeFraction is the historical share of a book's chapters the QA sweep
// flags for re-transcription (~2-4% across the validated books). Retranscribing only
// redoes that flagged subset, not the whole book, so before it has a progress row the
// ETA estimates the subset from this fraction rather than the full chapter count
// (which would wildly over-estimate the stage).
const retranscribeFraction = 0.04

// chapterStages are the per-chapter stages whose progress Total reveals a book's real
// chapter count. Only inspect sets books.chapters, so a book that predates that column
// (or is mid-flight before inspect recorded it) reports 0 forever; deriving the count
// from any of these stages' progress rows keeps the per-chapter ETA honest.
var chapterStages = []state.State{state.Splitting, state.ASR, state.Sanitizing}

// ChaptersFromProgress derives a book's chapter count from the largest Total among its
// per-chapter stages' progress rows, or 0 when none has a row yet. The scheduler uses
// it as the fallback when books.chapters is 0, so the per-chapter ETA stages don't all
// assume DefaultChapters. Pure.
func ChaptersFromProgress(progress map[string]Progress) int {
	best := 0
	for _, s := range chapterStages {
		if p, ok := progress[string(s)]; ok && p.Total > best {
			best = p.Total
		}
	}
	return best
}

// retranscribeSubset estimates how many chapters retranscribing will redo before it has
// a progress row: ~retranscribeFraction of the book's chapters (min 1). An unknown
// chapter count falls back to DefaultChapters as the base.
func retranscribeSubset(chapters int) int {
	if chapters <= 0 {
		chapters = DefaultChapters
	}
	n := int(math.Round(float64(chapters) * retranscribeFraction))
	if n < 1 {
		return 1
	}
	return n
}

// unitKind classifies how a stage's cost scales.
type unitKind int

const (
	unitBook    unitKind = iota // one unit per run (most stages)
	unitChapter                 // one unit per chapter
	unitChunk                   // one unit per fact-pass chunk
)

// stageUnit is a stage's cost model: the quantity it scales with and the seed
// seconds-per-unit prior used until an observed rate exists. The seeds are the
// M1-Ultra historical basis documented in M6-DESIGN.md.
type stageUnit struct {
	kind unitKind
	seed float64
}

var stageUnits = map[state.State]stageUnit{
	state.Inspecting:         {unitBook, 5},
	state.MarkersNormalizing: {unitBook, 180},
	state.Splitting:          {unitChapter, 4},
	state.ASR:                {unitChapter, 36},
	state.Sanitizing:         {unitChapter, 1},
	state.QASweep:            {unitBook, 10},
	state.QAAdjudicating:     {unitBook, 300},
	state.Retranscribing:     {unitChapter, 60},
	state.SpellingResearch:   {unitBook, 600},
	state.Correcting:         {unitBook, 15},
	state.FactPass:           {unitChunk, 900},
	state.Synthesizing:       {unitBook, 700},
	state.Validating:         {unitBook, 10},
	state.Auditing:           {unitBook, 700},
	state.Fixing:             {unitBook, 240},
	state.Contributing:       {unitBook, 1},
}

// Progress mirrors one store progress row (done/total) for a stage.
type Progress struct {
	Done  int
	Total int
}

// Book is the ETA engine's view of a pipeline book: enough to route its remaining
// path and place it in the queue simulation. It is framework-free (no store or
// scheduler import) so the scheduler builds it from a store.Book + progress rows.
type Book struct {
	ID        int64
	State     state.State
	Status    state.Status
	Series    string
	SeriesPos string
	// Chapters is the manifest chapter count (0 = unknown -> DefaultChapters).
	Chapters int
	// Progress is the per-stage done/total keyed by stage name (may be nil).
	Progress map[string]Progress
	// MaxAgentsPerBook is the fan-out width for supported stages. Zero preserves
	// the historical serial assumption for direct callers.
	MaxAgentsPerBook int
}

// UnitIsBook reports whether a stage is measured in whole books (one unit per run)
// rather than per-chapter or per-chunk. The scheduler uses it to record 1 unit for a
// book stage instead of deriving units from progress ticks. Unknown stages report
// false.
func UnitIsBook(stage string) bool {
	su, ok := stageUnits[state.State(stage)]
	return ok && su.kind == unitBook
}

// Observe folds a stage's observed unit-seconds into its rate via EWMA. current is
// the live per-stage rate map (seconds per unit); when the stage has no entry the
// in-code seed prior is the "old" value. It returns the updated rate and true when
// the caller should persist it, or (0, false) when the observation must be skipped:
// an unknown stage, non-positive units, or non-positive wallclock.
func Observe(stage string, wallclockSeconds float64, units int, current map[string]float64) (float64, bool) {
	su, ok := stageUnits[state.State(stage)]
	if !ok {
		return 0, false
	}
	if units <= 0 || wallclockSeconds <= 0 {
		return 0, false
	}
	observed := wallclockSeconds / float64(units)
	old := su.seed
	if r, has := current[stage]; has {
		old = r
	}
	return Alpha*observed + (1-Alpha)*old, true
}

// segment is one lane-bound piece of a book's remaining path: the stage, the lane it
// runs in, and its estimated seconds.
type segment struct {
	stage   state.State
	lane    state.Lane
	seconds float64
}

// rateFor returns the seconds-per-unit for a stage: the observed rate when present
// and positive, else the in-code seed.
func rateFor(s state.State, rates map[string]float64) float64 {
	if r, ok := rates[string(s)]; ok && r > 0 {
		return r
	}
	return stageUnits[s].seed
}

// stageTotalUnits is the full unit count for a stage of book b (ignoring any partial
// progress): 1 for a book stage, b.Chapters (or the progress total, or
// DefaultChapters) for a chapter stage, the progress total (or DefaultChunks) for
// the fact-pass chunk stage.
func stageTotalUnits(b Book, s state.State) int {
	switch stageUnits[s].kind {
	case unitChapter:
		// Retranscribing only redoes the QA-flagged subset, so it estimates from that
		// subset rather than the full chapter count. A progress row (the stage has
		// started) carries the real subset total and is authoritative either way.
		if s == state.Retranscribing {
			if p, ok := b.Progress[string(s)]; ok && p.Total > 0 {
				return p.Total
			}
			return retranscribeSubset(b.Chapters)
		}
		if b.Chapters > 0 {
			return b.Chapters
		}
		if p, ok := b.Progress[string(s)]; ok && p.Total > 0 {
			return p.Total
		}
		return DefaultChapters
	case unitChunk:
		if p, ok := b.Progress[string(s)]; ok && p.Total > 0 {
			return p.Total
		}
		return DefaultChunks
	default: // unitBook
		return 1
	}
}

// stageRemainingUnits is the units still to do for a stage of book b. For the book's
// CURRENT stage with a progress row (total > 0) the remainder is total - done; every
// other case is the full estimate.
func stageRemainingUnits(b Book, s state.State, isCurrent bool) int {
	if isCurrent {
		if p, ok := b.Progress[string(s)]; ok && p.Total > 0 {
			rem := p.Total - p.Done
			if rem < 0 {
				rem = 0
			}
			return rem
		}
	}
	return stageTotalUnits(b, s)
}

// remainingSegments builds the ordered lane-bound segments of a book's remaining
// optimistic path. A waypoint (queued/ready) contributes no cost - its next stage
// starts fresh. The book's own current stage uses its progress-derived remainder;
// every later stage uses its full estimate. Returns nil for a terminal or a
// status != none (paused/parked/failed) book.
func remainingSegments(b Book, rates map[string]float64) []segment {
	if b.Status != state.StatusNone || state.IsTerminal(b.State) {
		return nil
	}
	var segs []segment
	// The remaining path is optimistic: state.MainlineNext takes the happy branch at
	// every fork (skipping the conditional stages and routing the off-mainline stages
	// back onto the mainline), so following it from any start is finite - every fork
	// moves toward done and no off-mainline stage is re-entered.
	for cur := b.State; cur != "" && !state.IsTerminal(cur); cur = state.MainlineNext(cur) {
		if state.IsWaypoint(cur) {
			continue // no lane, no cost
		}
		isCurrent := cur == b.State
		units := stageRemainingUnits(b, cur, isCurrent)
		seconds := float64(units) * rateFor(cur, rates)
		if state.SupportsAgentFanout(cur) && units > 0 {
			fanout := b.MaxAgentsPerBook
			if fanout < 1 {
				fanout = 1
			}
			waves := (units + fanout - 1) / fanout
			seconds = float64(waves) * rateFor(cur, rates)
			if cur == state.FactPass && b.MaxAgentsPerBook > 0 {
				seconds += 300
			} // bounded serial notes assembly
		}
		segs = append(segs, segment{
			stage:   cur,
			lane:    state.LaneOf(cur),
			seconds: seconds,
		})
	}
	return segs
}

// BookETA returns the estimated seconds a book still needs (the sum of its remaining
// segments), and false when it has no ETA: terminal, or status != none
// (paused/parked/failed).
func BookETA(b Book, rates map[string]float64) (float64, bool) {
	segs := remainingSegments(b, rates)
	if b.Status != state.StatusNone || state.IsTerminal(b.State) {
		return 0, false
	}
	total := 0.0
	for _, s := range segs {
		total += s.seconds
	}
	return total, true
}
