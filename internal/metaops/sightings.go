package metaops

import (
	"context"
	"log/slog"
	"time"
)

// SightingRecorder is the durable sighting store the scan manager writes to when
// a scan completes. It is injected (WithSightings) so metaops never imports
// store, like OverrideLookup and the override PersistFunc.
//
// It is deliberately write-plus-one-question: the scan manager only records what
// it walked and asks whether anything was ever recorded before. Reading the rows
// back is the API's job (handleGetScan), so nothing sighting-shaped has to ride
// the job snapshot or the scan cache.
type SightingRecorder interface {
	// RecordSightings records every path as seen at `at`, preserving the
	// first_seen_at and baseline flag of a path that is already known.
	RecordSightings(ctx context.Context, paths []string, at time.Time, baseline bool) error
	// HasSightings reports whether anything has ever been recorded, which answers
	// the baseline question ("is this the first batch?").
	HasSightings(ctx context.Context) (bool, error)
}

// WithSightings records, on every completed scan, which library folders exist -
// so the Library tab can show what appeared since the user last looked. nil (the
// default) disables the feature entirely: no rows are written and every book
// reports is_new false.
func WithSightings(rec SightingRecorder) ScanManagerOption {
	return func(m *ScanManager) { m.sightings = rec }
}

// ComputeIsNew is the read-time "this book is new to the library" predicate. The
// caller supplies the book's stored sighting facts: baseline (this path came from
// the first batch ever recorded, i.e. the library as it already stood) and
// acknowledged (the user dismissed it from the New view).
//
// It is derived at READ time rather than stored, because three of its four inputs
// change independently of a scan: a book gets enqueued (pipeline_book), hidden,
// or dismissed (acknowledged) without the library being re-walked, and a stored
// flag would then be stale until the next scan.
func ComputeIsNew(b ScannedBook, baseline, acknowledged bool) bool {
	return !baseline && // not part of the pre-existing library
		!acknowledged && // not dismissed
		b.PipelineBook == nil && // not already queued/processed here
		!b.Hidden // not hidden from the candidate list
}

// recordJobSightings records every candidate path of a COMPLETED job.
//
// The FIRST batch ever recorded becomes the BASELINE: on an upgrade (or a fresh
// install pointed at an existing library) every folder the user already owns is
// recorded as not-new, so the New view starts empty instead of reporting the
// whole library. The seed prefers the cached last scan (see restoreCache), so a
// daemon that restarts before its next scan still baselines the library it knew.
//
// Nothing is read back onto the job: first_seen_at and is_new are attached by the
// API on every read (handleGetScan), so the snapshot - and therefore the cache -
// never has to carry them.
//
// Failures are logged and swallowed: a sighting is a nicety on top of the scan,
// and it must never turn a completed library walk into a failed one.
func (m *ScanManager) recordJobSightings(job *scanJob) {
	if m.sightings == nil {
		return
	}
	m.mu.Lock()
	paths := make([]string, 0, len(job.books))
	for _, b := range job.books {
		paths = append(paths, b.SourcePath)
	}
	at := job.startedAt
	m.mu.Unlock()
	if len(paths) == 0 {
		return
	}

	// The I/O runs WITHOUT the manager lock: it is a database round trip on the
	// path that also serves the ~700ms Library poll.
	has, err := m.sightings.HasSightings(m.ctx)
	if err != nil {
		slog.Warn("scan: could not read library sightings; new-book marking skipped", "err", err)
		return
	}
	if err := m.sightings.RecordSightings(m.ctx, paths, at, !has); err != nil {
		slog.Warn("scan: could not record library sightings", "err", err)
	}
}
