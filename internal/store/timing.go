package store

import (
	"context"
	"database/sql"
	"time"
)

// BookTiming separates end-to-end batch elapsed from actual execution and the
// stable post-primary-ASR baseline. Durations include all attempts; superseded
// runs remain real elapsed/active work.
type BookTiming struct {
	BatchStartedAt          string  `json:"batch_started_at,omitempty"`
	PrimaryASRCompletedAt   string  `json:"primary_asr_completed_at,omitempty"`
	BatchElapsedSeconds     float64 `json:"batch_elapsed_seconds,omitempty"`
	PreASRWallSeconds       float64 `json:"pre_asr_wall_seconds,omitempty"`
	ASRActiveSeconds        float64 `json:"asr_active_seconds,omitempty"`
	PostASRElapsedSeconds   float64 `json:"post_asr_elapsed_seconds,omitempty"`
	ActiveProcessingSeconds float64 `json:"active_processing_seconds,omitempty"`
	QueueWaitSeconds        float64 `json:"queue_wait_seconds,omitempty"`
}

type timingRun struct {
	stage, started, finished string
	ok                       sql.NullInt64
	superseded               bool
}

// BookTimings computes timing summaries for the catalogue in one ordered query.
// Open intervals end at the injected store clock. A done book freezes at its last
// finished run; active/paused/parked books continue end-to-end/post-ASR elapsed to
// now, matching their wall-clock experience.
func (db *DB) BookTimings(ctx context.Context) (map[int64]BookTiming, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT b.id,b.state,r.stage,r.started_at,r.finished_at,r.ok,COALESCE(r.superseded,0)
		FROM books b LEFT JOIN stage_runs r ON r.book_id=b.id ORDER BY b.id,r.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type group struct {
		state string
		runs  []timingRun
	}
	groups := map[int64]*group{}
	for rows.Next() {
		var id int64
		var state string
		var stage, started sql.NullString
		var finished sql.NullString
		var ok sql.NullInt64
		var superseded int
		if err := rows.Scan(&id, &state, &stage, &started, &finished, &ok, &superseded); err != nil {
			return nil, err
		}
		g := groups[id]
		if g == nil {
			g = &group{state: state}
			groups[id] = g
		}
		if stage.Valid {
			g.runs = append(g.runs, timingRun{stage: stage.String, started: started.String, finished: finished.String, ok: ok, superseded: superseded == 1})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := nowFn().UTC()
	out := make(map[int64]BookTiming, len(groups))
	for id, g := range groups {
		out[id] = computeBookTiming(g.runs, g.state == "done", now)
	}
	return out, nil
}

func (db *DB) BookTiming(ctx context.Context, bookID int64) (BookTiming, error) {
	var state string
	if err := db.sql.QueryRowContext(ctx, `SELECT state FROM books WHERE id=?`, bookID).Scan(&state); err != nil {
		return BookTiming{}, err
	}
	rows, err := db.sql.QueryContext(ctx, `SELECT stage,started_at,finished_at,ok,COALESCE(superseded,0)
		FROM stage_runs WHERE book_id=? ORDER BY id`, bookID)
	if err != nil {
		return BookTiming{}, err
	}
	defer func() { _ = rows.Close() }()
	var runs []timingRun
	for rows.Next() {
		var r timingRun
		var finished sql.NullString
		var superseded int
		if err := rows.Scan(&r.stage, &r.started, &finished, &r.ok, &superseded); err != nil {
			return BookTiming{}, err
		}
		if finished.Valid {
			r.finished = finished.String
		}
		r.superseded = superseded == 1
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return BookTiming{}, err
	}
	return computeBookTiming(runs, state == "done", nowFn().UTC()), nil
}

func computeBookTiming(runs []timingRun, done bool, now time.Time) BookTiming {
	if len(runs) == 0 {
		return BookTiming{}
	}
	parse := func(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t }
	first := parse(runs[0].started)
	end := now
	lastFinished := time.Time{}
	active := time.Duration(0)
	asrActive := time.Duration(0)
	asrDone := time.Time{}
	for _, r := range runs {
		start := parse(r.started)
		finish := parse(r.finished)
		intervalEnd := finish
		if intervalEnd.IsZero() {
			intervalEnd = now
		}
		if intervalEnd.After(start) {
			d := intervalEnd.Sub(start)
			active += d
			if r.stage == "asr" {
				asrActive += d
			}
		}
		if finish.After(lastFinished) {
			lastFinished = finish
		}
		if r.stage == "asr" && r.ok.Valid && r.ok.Int64 == 1 && !r.superseded && finish.After(asrDone) {
			asrDone = finish
		}
	}
	if done && !lastFinished.IsZero() {
		end = lastFinished
	}
	t := BookTiming{BatchStartedAt: first.UTC().Format(timeLayout), BatchElapsedSeconds: max(0, end.Sub(first).Seconds()), ASRActiveSeconds: asrActive.Seconds(), ActiveProcessingSeconds: active.Seconds()}
	if !asrDone.IsZero() {
		t.PrimaryASRCompletedAt = asrDone.UTC().Format(timeLayout)
		t.PreASRWallSeconds = max(0, asrDone.Sub(first).Seconds())
		t.PostASRElapsedSeconds = max(0, end.Sub(asrDone).Seconds())
	}
	t.QueueWaitSeconds = max(0, t.BatchElapsedSeconds-t.ActiveProcessingSeconds)
	return t
}
