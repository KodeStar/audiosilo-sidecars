package eta

import (
	"sort"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

// LaneCaps is the per-lane worker capacity the queue simulation runs under. The
// scheduler owns the real values (its own asr/mechanical constants and the
// configured agent-book concurrency) and passes them in, so eta keeps no private copy
// that could drift from the scheduler's. Each field is clamped to >= 1 by QueueETA.
type LaneCaps struct {
	ASR              int
	Mechanical       int
	Agent            int
	AgentInvocations int
}

// atLeastOne clamps a lane capacity to a minimum of 1, so a zero/negative cap never
// stalls the simulation (a lane must be able to run at least one book).
func atLeastOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// simBook is one book's mutable state inside the queue simulation: its ordered
// segments and how far it has progressed (idx), whether it currently occupies a lane
// slot, and when that running segment finishes.
type simBook struct {
	id         int64
	series     string
	pos        float64
	seriesRank int
	segs       []segment
	idx        int
	running    bool
	finishAt   float64
}

// done reports whether the book has consumed all its segments.
func (sb *simBook) done() bool { return sb.idx >= len(sb.segs) }

// cur returns the book's current (next-to-run) segment; only valid when !done().
func (sb *simBook) cur() segment { return sb.segs[sb.idx] }

// holdsSeriesLock reports whether this book still holds its series lock, using the
// SAME predicate as the real scheduler (state.HoldsSeriesLock: the book's current stage
// is before Ready) rather than re-deriving it from "any remaining agent segment", so
// the simulation's series gating cannot drift from the scheduler's. A book with no
// remaining segments (done) holds nothing. Since every agent stage precedes Ready this
// is equivalent to the old any-remaining-agent test today, but it is now the one shared
// rule.
func (sb *simBook) holdsSeriesLock() bool {
	if sb.done() {
		return false
	}
	return state.HoldsSeriesLock(sb.cur().stage)
}

// QueueETA returns the wall-clock makespan (seconds) to drain the active queue: a
// greedy, event-driven simulation over the three lanes. It considers only books with
// status none and a non-terminal state; each is a sequence of lane-bound segments
// run strictly in order. Lane capacities come from caps (each clamped to >= 1). The
// ASR lane serves retranscribing before other work then breadth-first across series;
// the agent lane
// runs a book only while it holds its series lock (the lowest-position book with
// agent work remaining per series; seriesless books are always eligible), recomputed
// as books progress. It is pure and reads no clock.
func QueueETA(books []Book, rates map[string]float64, caps LaneCaps) float64 {
	seriesItems := make([]state.SeriesQueueItem, 0, len(books))
	for _, b := range books {
		seriesItems = append(seriesItems, state.SeriesQueueItem{ID: b.ID, Series: b.Series, SeriesPos: b.SeriesPos})
	}
	seriesRanks := state.SeriesBreadthRanks(seriesItems)
	sim := make([]*simBook, 0, len(books))
	for _, b := range books {
		// BookETA describes an isolated book and may use its full fan-out. QueueETA
		// must also respect the daemon-wide invocation pool. Dividing that pool over
		// the simultaneously admitted agent books is exact for the modern default
		// (queue * per-book == global) and deliberately conservative for a legacy
		// single concurrency value, where global == queue rather than queue squared.
		if caps.AgentInvocations > 0 && b.MaxAgentsPerBook > 0 {
			shared := atLeastOne(caps.AgentInvocations / atLeastOne(caps.Agent))
			b.MaxAgentsPerBook = min(b.MaxAgentsPerBook, shared)
		}
		segs := remainingSegments(b, rates)
		if len(segs) == 0 {
			continue
		}
		sim = append(sim, &simBook{
			id: b.ID, series: strings.TrimSpace(b.Series), pos: state.ParseSeriesPos(b.SeriesPos),
			seriesRank: seriesRanks[b.ID], segs: segs,
		})
	}
	if len(sim) == 0 {
		return 0
	}

	laneCap := map[state.Lane]int{
		state.LaneASR:        atLeastOne(caps.ASR),
		state.LaneMechanical: atLeastOne(caps.Mechanical),
		state.LaneAgent:      atLeastOne(caps.Agent),
	}

	now := 0.0
	for {
		startWaiting(sim, laneCap, now)

		// Find the earliest running finish; if nothing runs, the queue is drained.
		next, running := earliestFinish(sim)
		if !running {
			return now
		}
		now = next
		for _, sb := range sim {
			if sb.running && sb.finishAt <= now {
				sb.running = false
				sb.idx++ // segment complete; advance to the next (or done)
			}
		}
	}
}

// startWaiting fills every lane up to capacity with eligible waiting books at time
// now, mutating sim in place.
func startWaiting(sim []*simBook, caps map[state.Lane]int, now float64) {
	free := map[state.Lane]int{}
	for lane, capacity := range caps {
		free[lane] = capacity
	}
	for _, sb := range sim {
		if sb.running {
			free[sb.cur().lane]--
		}
	}
	holders := lockHolders(sim)

	// Gather waiting candidates per lane.
	waiting := map[state.Lane][]*simBook{}
	for _, sb := range sim {
		if sb.running || sb.done() {
			continue
		}
		lane := sb.cur().lane
		if lane == state.LaneAgent && !holders[sb.id] {
			continue // gated by the series lock
		}
		waiting[lane] = append(waiting[lane], sb)
	}

	for lane, cands := range waiting {
		sortLane(lane, cands)
		for _, sb := range cands {
			if free[lane] <= 0 {
				break
			}
			sb.running = true
			sb.finishAt = now + sb.cur().seconds
			free[lane]--
		}
	}
}

// sortLane orders a lane's waiting candidates: the ASR lane puts retranscribing
// first, then ordinary ASR by series breadth rank and id; other lanes are FIFO.
func sortLane(lane state.Lane, cands []*simBook) {
	if lane == state.LaneASR {
		sort.Slice(cands, func(i, j int) bool {
			ri := cands[i].cur().stage == state.Retranscribing
			rj := cands[j].cur().stage == state.Retranscribing
			if ri != rj {
				return ri
			}
			if !ri && cands[i].seriesRank != cands[j].seriesRank {
				return cands[i].seriesRank < cands[j].seriesRank
			}
			return cands[i].id < cands[j].id
		})
		return
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].id < cands[j].id })
}

// lockHolders returns the set of book ids permitted to run agent work now: the
// lowest-position book with agent work remaining in each series, plus every
// seriesless book with agent work remaining. A book with no remaining agent work is
// omitted (it needs no lock and blocks no one). Recomputed each scheduling pass, so
// a series' lock passes to its next book as the current holder finishes its agent
// work.
func lockHolders(sim []*simBook) map[int64]bool {
	holders := map[int64]bool{}
	best := map[string]*simBook{}
	for _, sb := range sim {
		if !sb.holdsSeriesLock() {
			continue
		}
		if sb.series == "" {
			holders[sb.id] = true
			continue
		}
		cur, ok := best[sb.series]
		if !ok || simSeriesLess(sb, cur) {
			best[sb.series] = sb
		}
	}
	for _, sb := range best {
		holders[sb.id] = true
	}
	return holders
}

// simSeriesLess orders two books of the same series by numeric position then id.
func simSeriesLess(a, b *simBook) bool {
	if a.pos != b.pos {
		return a.pos < b.pos
	}
	return a.id < b.id
}

// earliestFinish returns the smallest finishAt among running books and whether any
// book is running.
func earliestFinish(sim []*simBook) (float64, bool) {
	var earliest float64
	found := false
	for _, sb := range sim {
		if !sb.running {
			continue
		}
		if !found || sb.finishAt < earliest {
			earliest, found = sb.finishAt, true
		}
	}
	return earliest, found
}
