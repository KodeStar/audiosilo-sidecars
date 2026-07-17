package eta

import (
	"math"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("makespan = %v, want %v", got, want)
	}
}

// caps builds the LaneCaps these tests run under: the scheduler's fixed ASR 1 /
// mechanical 2, with the agent capacity varied per case.
func caps(agent int) LaneCaps { return LaneCaps{ASR: 1, Mechanical: 2, Agent: agent} }

func TestQueueETAEmpty(t *testing.T) {
	if got := QueueETA(nil, nil, caps(2)); got != 0 {
		t.Errorf("empty queue = %v, want 0", got)
	}
	// Only terminal/paused books contribute nothing.
	books := []Book{
		{ID: 1, State: state.Done},
		{ID: 2, State: state.ASR, Status: state.StatusPaused, Chapters: 5},
	}
	if got := QueueETA(books, nil, caps(2)); got != 0 {
		t.Errorf("all-inactive queue = %v, want 0", got)
	}
}

func TestQueueETASingleBookEqualsBookETA(t *testing.T) {
	// With no contention, the makespan of a lone book equals its BookETA.
	b := Book{ID: 1, State: state.Validating}
	want, _ := BookETA(b, nil)
	approx(t, QueueETA([]Book{b}, nil, caps(2)), want)
}

func TestQueueETAMechanicalParallelism(t *testing.T) {
	// Books at contributing have a single mechanical segment (seed 1s). The mechanical
	// lane has capacity 2, so N books drain in ceil(N/2) seconds.
	mk := func(n int) []Book {
		out := make([]Book, n)
		for i := range out {
			out[i] = Book{ID: int64(i + 1), State: state.Contributing}
		}
		return out
	}
	approx(t, QueueETA(mk(2), nil, caps(4)), 1) // 2 in parallel
	approx(t, QueueETA(mk(3), nil, caps(4)), 2) // 2 then 1
	approx(t, QueueETA(mk(4), nil, caps(4)), 2) // two waves of 2
	approx(t, QueueETA(mk(5), nil, caps(4)), 3) // three waves
}

func TestQueueETASeriesLockSerializesAgent(t *testing.T) {
	// Two books at auditing: one agent segment (seed 700) + a contributing tail (1).
	// Same series -> the series lock forces them through the agent lane one at a time
	// even though agentCap is 2, so the makespan is 700 + 700 + 1.
	same := []Book{
		{ID: 1, State: state.Auditing, Series: "S", SeriesPos: "1"},
		{ID: 2, State: state.Auditing, Series: "S", SeriesPos: "2"},
	}
	approx(t, QueueETA(same, nil, caps(2)), 700+700+1)

	// Different series (agentCap 2) -> both audit in parallel, then contributing: 701.
	diff := []Book{
		{ID: 1, State: state.Auditing, Series: "A", SeriesPos: "1"},
		{ID: 2, State: state.Auditing, Series: "B", SeriesPos: "1"},
	}
	approx(t, QueueETA(diff, nil, caps(2)), 700+1)

	// Seriesless books are always eligible -> parallel like the different-series case.
	none := []Book{
		{ID: 1, State: state.Auditing},
		{ID: 2, State: state.Auditing},
	}
	approx(t, QueueETA(none, nil, caps(2)), 700+1)
}

func TestQueueETAAgentCapLimitsParallelism(t *testing.T) {
	// Three different-series audit books with agentCap 1 serialize on the agent lane:
	// 700*3 + a final 1s contributing tail.
	books := []Book{
		{ID: 1, State: state.Auditing, Series: "A", SeriesPos: "1"},
		{ID: 2, State: state.Auditing, Series: "B", SeriesPos: "1"},
		{ID: 3, State: state.Auditing, Series: "C", SeriesPos: "1"},
	}
	approx(t, QueueETA(books, nil, caps(1)), 700*3+1)
}

func TestSortLaneRetranscribePriority(t *testing.T) {
	// The ASR lane serves retranscribing before ordinary ASR, then FIFO by id.
	cands := []*simBook{
		{id: 3, segs: []segment{{stage: state.ASR, lane: state.LaneASR}}},
		{id: 1, segs: []segment{{stage: state.Retranscribing, lane: state.LaneASR}}},
		{id: 2, segs: []segment{{stage: state.ASR, lane: state.LaneASR}}},
	}
	sortLane(state.LaneASR, cands)
	gotIDs := []int64{cands[0].id, cands[1].id, cands[2].id}
	want := []int64{1, 2, 3} // retranscribe (id 1) first, then ASR by id
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("ASR order = %v, want %v (retranscribe first, then id)", gotIDs, want)
		}
	}

	// A non-ASR lane is plain FIFO by id.
	mech := []*simBook{
		{id: 5, segs: []segment{{lane: state.LaneMechanical}}},
		{id: 2, segs: []segment{{lane: state.LaneMechanical}}},
	}
	sortLane(state.LaneMechanical, mech)
	if mech[0].id != 2 || mech[1].id != 5 {
		t.Errorf("mechanical order = [%d %d], want [2 5]", mech[0].id, mech[1].id)
	}
}

func TestLockHolders(t *testing.T) {
	agentSeg := segment{stage: state.Auditing, lane: state.LaneAgent}
	mechSeg := segment{stage: state.Contributing, lane: state.LaneMechanical}
	sim := []*simBook{
		// Series S: pos 2 and pos 1 both have agent work; only pos 1 holds the lock.
		{id: 1, series: "S", pos: 2, segs: []segment{agentSeg}},
		{id: 2, series: "S", pos: 1, segs: []segment{agentSeg}},
		// Seriesless with agent work -> always a holder.
		{id: 3, segs: []segment{agentSeg}},
		// No remaining agent work -> not a holder (needs no lock, blocks no one).
		{id: 4, series: "T", pos: 1, segs: []segment{mechSeg}},
		// Done book -> ignored.
		{id: 5, series: "S", pos: 0, segs: []segment{agentSeg}, idx: 1},
	}
	holders := lockHolders(sim)
	if holders[1] {
		t.Error("series S pos 2 should NOT hold the lock")
	}
	if !holders[2] {
		t.Error("series S pos 1 should hold the lock")
	}
	if !holders[3] {
		t.Error("seriesless agent book should hold the lock")
	}
	if holders[4] {
		t.Error("book with no remaining agent work should not be a holder")
	}
	if holders[5] {
		t.Error("done book should never be a holder")
	}
}

func TestHoldsSeriesLock(t *testing.T) {
	// The sim uses the scheduler's own predicate (current stage < Ready), so a book at a
	// pre-Ready stage holds the lock and one that has advanced to a post-Ready
	// (contributing) segment has released it.
	sb := &simBook{segs: []segment{
		{stage: state.Validating, lane: state.LaneMechanical},
		{stage: state.Auditing, lane: state.LaneAgent},
		{stage: state.Contributing, lane: state.LaneMechanical},
	}}
	if !sb.holdsSeriesLock() {
		t.Error("pre-Ready current stage -> want true")
	}
	sb.idx = 2 // now at contributing (post-Ready)
	if sb.holdsSeriesLock() {
		t.Error("post-Ready current stage -> want false (lock released)")
	}
}
