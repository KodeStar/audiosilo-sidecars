package scheduler

import (
	"context"
	"math"

	"github.com/kodestar/audiosilo-sidecars/internal/eta"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// publishETAs recomputes and publishes the daemon-wide ETA snapshot from the fresh
// book list dispatch already advanced: a per-book remaining-seconds estimate (active,
// unparked books only) and the queue makespan. It is deduped exactly like
// queue.stats - every value is rounded to 10s before the snapshot comparison, and it
// publishes eta.update only on a change - so a stable idle daemon emits no frames.
// The rounded per-book snapshot doubles as the API getters' source.
//
// dispatch runs this on every wake AND every 5s idle tick. When no book is active
// (every book terminal/paused/parked/failed) there is nothing to estimate, so it
// skips the progress query and the queue simulation entirely and publishes a single
// empty snapshot (deduped, so an idle daemon clears the ETA once and then stays
// silent). Only an active pass pays for the full recompute.
func (s *Scheduler) publishETAs(ctx context.Context, books []store.Book) {
	if !anyActiveBook(books) {
		s.publishEmptyETAs()
		return
	}

	progressByBook, err := s.db.ListAllProgress(ctx)
	if err != nil {
		return
	}
	rates := s.rateSnapshot()

	inputs := make([]eta.Book, 0, len(books))
	fanout := 1
	if v, ok := s.exec.(interface{ AgentMaxPerBook() int }); ok {
		fanout = max(1, v.AgentMaxPerBook())
	}
	for _, b := range books {
		inputs = append(inputs, toETABook(b, progressByBook[b.ID], fanout))
	}

	// inputs are already in book-id order (dispatch's book list is ordered by id), so
	// the payload it produces is id-ordered too - stable output for the dedup compare.
	perBook := map[int64]int64{}
	payload := make([]map[string]any, 0, len(inputs))
	for _, in := range inputs {
		secs, ok := eta.BookETA(in, rates)
		if !ok {
			continue
		}
		r := round10(secs)
		perBook[in.ID] = r
		payload = append(payload, map[string]any{"book_id": in.ID, "eta_seconds": r})
	}
	queue := round10(eta.QueueETA(inputs, rates, eta.LaneCaps{
		ASR: asrCapacity, Mechanical: mechCapacity, Agent: s.agentCap,
		AgentInvocations: agentInvocationCapacity(s.exec, s.agentCap),
	}))

	s.etaMu.Lock()
	changed := !s.haveETA || s.queueETA != queue || !sameETA(s.bookETA, perBook)
	if changed {
		s.bookETA = perBook
		s.queueETA = queue
		s.haveETA = true
	}
	s.etaMu.Unlock()
	if !changed {
		return
	}
	_ = s.hub.Publish("eta.update", map[string]any{
		"queue_seconds": queue,
		"books":         payload,
	})
}

func agentInvocationCapacity(exec Executor, fallback int) int {
	if runtime, ok := exec.(agentInvocationRuntime); ok {
		_, _, capacity := runtime.AgentInvocationRuntime()
		if capacity > 0 {
			return capacity
		}
	}
	return max(1, fallback)
}

// anyActiveBook reports whether any book is still schedulable (status none and
// non-terminal), i.e. whether an ETA recompute has anything to estimate. It mirrors
// eta.BookETA's own gate, so a false result means every book's BookETA would return
// no-ETA anyway - the empty snapshot is exact, not an approximation.
func anyActiveBook(books []store.Book) bool {
	for _, b := range books {
		if b.Status == "" && !state.IsTerminal(state.State(b.State)) {
			return true
		}
	}
	return false
}

// publishEmptyETAs clears the stored ETA snapshot and publishes a single empty
// eta.update, deduped: when the snapshot is already empty it does nothing (so an
// idle daemon emits at most one clearing frame). queue_seconds is JSON null (not 0)
// when there is nothing to estimate, so a client can tell "no active queue" from a
// genuinely-computed zero makespan; the active path (publishETAs) stays numeric.
func (s *Scheduler) publishEmptyETAs() {
	s.etaMu.Lock()
	changed := !s.haveETA || s.queueETA != 0 || len(s.bookETA) != 0
	if changed {
		s.bookETA = map[int64]int64{}
		s.queueETA = 0
		s.haveETA = true
	}
	s.etaMu.Unlock()
	if !changed {
		return
	}
	_ = s.hub.Publish("eta.update", map[string]any{
		"queue_seconds": (*int64)(nil),
		"books":         []map[string]any{},
	})
}

// ETASeconds returns a book's latest estimated remaining seconds (rounded to 10s),
// and false when the book has no current ETA (terminal, paused, parked, or failed,
// or the first ETA pass has not run yet). The API reads it here - it never computes.
func (s *Scheduler) ETASeconds(bookID int64) (int64, bool) {
	s.etaMu.Lock()
	defer s.etaMu.Unlock()
	v, ok := s.bookETA[bookID]
	return v, ok
}

// ETASnapshot returns a copy of the whole per-book ETA map under a SINGLE lock, so
// the book-list handler reads every book's ETA once instead of taking etaMu per book
// (ETASeconds) across the loop. A book absent from the map has no active ETA. The
// detail path keeps using ETASeconds for its single lookup.
func (s *Scheduler) ETASnapshot() map[int64]int64 {
	s.etaMu.Lock()
	defer s.etaMu.Unlock()
	out := make(map[int64]int64, len(s.bookETA))
	for id, v := range s.bookETA {
		out[id] = v
	}
	return out
}

// rateSnapshot returns a copy of the per-stage rate cache safe to hand to the pure
// eta functions.
func (s *Scheduler) rateSnapshot() map[string]float64 {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	out := make(map[string]float64, len(s.rates))
	for k, v := range s.rates {
		out[k] = v
	}
	return out
}

// toETABook adapts a store.Book + its progress rows into the pure eta.Book input.
func toETABook(b store.Book, progress []store.Progress, fanout int) eta.Book {
	pm := make(map[string]eta.Progress, len(progress))
	for _, p := range progress {
		pm[p.Stage] = eta.Progress{Done: p.Done, Total: p.Total}
	}
	// books.chapters is 0 until inspect records it (and stays 0 for pre-M6 books), which
	// would make every per-chapter ETA stage assume DefaultChapters. Derive the real
	// count from any per-chapter stage's progress row as a fallback.
	chapters := b.Chapters
	if chapters == 0 {
		chapters = eta.ChaptersFromProgress(pm)
	}
	return eta.Book{
		ID:               b.ID,
		State:            state.State(b.State),
		Status:           state.Status(b.Status),
		Series:           b.Series,
		SeriesPos:        b.SeriesPos,
		Chapters:         chapters,
		Progress:         pm,
		MaxAgentsPerBook: max(1, fanout),
	}
}

// round10 rounds seconds to the nearest 10, the ETA snapshot's dedup/publish
// granularity.
func round10(seconds float64) int64 {
	if seconds <= 0 {
		return 0
	}
	return int64(math.Round(seconds/10) * 10)
}

// sameETA reports whether two per-book ETA snapshots are identical.
func sameETA(a, b map[int64]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for id, v := range a {
		if b[id] != v {
			return false
		}
	}
	return true
}
