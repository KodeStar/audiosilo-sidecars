package store

import (
	"context"
	"database/sql"
	"fmt"
)

// AgentInvocation is one concrete backend process attempt belonging to a parent
// stage run. It is the durable source of truth for child liveness and spend;
// stage_runs carries a compatible materialized aggregate.
type AgentInvocation struct {
	ID                  int64    `json:"id"`
	StageRunID          int64    `json:"stage_run_id"`
	BookID              int64    `json:"book_id"`
	Stage               string   `json:"stage"`
	WorkUnit            string   `json:"work_unit"`
	Backend             string   `json:"backend"`
	Model               string   `json:"model"`
	ProcessID           int      `json:"process_id,omitempty"`
	Active              bool     `json:"active"`
	HeartbeatAt         string   `json:"heartbeat_at"`
	ProgressAt          string   `json:"progress_at"`
	StartedAt           string   `json:"started_at"`
	CompletedAt         string   `json:"completed_at,omitempty"`
	Status              string   `json:"status"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	CostUSD             float64  `json:"cost_usd"`
	CostReported        bool     `json:"cost_reported"`
	EstimatedAPICostUSD *float64 `json:"estimated_api_cost_usd,omitempty"`
	EstimateComplete    bool     `json:"estimate_complete"`
	Error               string   `json:"error,omitempty"`
}

// StartAgentInvocation associates an invocation with the single open stage run.
func (db *DB) StartAgentInvocation(ctx context.Context, bookID int64, stage, workUnit, backend, model string) (int64, error) {
	var runID int64
	if err := db.sql.QueryRowContext(ctx, `SELECT id FROM stage_runs WHERE book_id=? AND stage=? AND finished_at IS NULL ORDER BY id DESC LIMIT 1`, bookID, stage).Scan(&runID); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("StartAgentInvocation: no open stage_run for book %d stage %q", bookID, stage)
		}
		return 0, err
	}
	now := timestamp(nowFn())
	res, err := db.sql.ExecContext(ctx, `INSERT INTO agent_invocations
		(stage_run_id,book_id,stage,work_unit,backend,model,heartbeat_at,progress_at,started_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, runID, bookID, stage, workUnit, backend, model, now, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TouchAgentInvocation updates one child's heartbeat and, optionally, progress.
// The parent heartbeat remains a roll-up for compatible supervisor consumers.
func (db *DB) TouchAgentInvocation(ctx context.Context, id int64, progress bool) error {
	now := timestamp(nowFn())
	q := `UPDATE agent_invocations SET heartbeat_at=? WHERE id=? AND active=1`
	if progress {
		q = `UPDATE agent_invocations SET heartbeat_at=?, progress_at=? WHERE id=? AND active=1`
		if _, err := db.sql.ExecContext(ctx, q, now, now, id); err != nil {
			return err
		}
	} else if _, err := db.sql.ExecContext(ctx, q, now, id); err != nil {
		return err
	}
	_, err := db.sql.ExecContext(ctx, `UPDATE stage_runs SET heartbeat_at=?, progress_at=CASE WHEN ? THEN ? ELSE progress_at END
		WHERE id=(SELECT stage_run_id FROM agent_invocations WHERE id=?)`, now, boolToInt(progress), now, id)
	return err
}

func (db *DB) SetAgentInvocationProcess(ctx context.Context, id int64, pid int, active bool) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := timestamp(nowFn())
	if _, err = tx.ExecContext(ctx, `UPDATE agent_invocations SET process_id=?,active=?,heartbeat_at=? WHERE id=?`, pid, boolToInt(active), now, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE stage_runs SET process_active=EXISTS(SELECT 1 FROM agent_invocations ai WHERE ai.stage_run_id=stage_runs.id AND ai.active=1),
		process_id=COALESCE((SELECT MAX(process_id) FROM agent_invocations ai WHERE ai.stage_run_id=stage_runs.id AND ai.active=1),0),heartbeat_at=?
		WHERE id=(SELECT stage_run_id FROM agent_invocations WHERE id=?)`, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishAgentInvocation records the immutable outcome and atomically rebuilds the
// parent stage summary from every child, preventing concurrent lost updates and
// making invocation rows the accounting source of truth.
func (db *DB) FinishAgentInvocation(ctx context.Context, id int64, status, model string, in, out, cached int64,
	providerCost float64, providerReported bool, estimated float64, estimatedKnown bool, errorMessage string,
) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var estimate any
	if estimatedKnown {
		estimate = estimated
	}
	finished := timestamp(nowFn())
	res, err := tx.ExecContext(ctx, `UPDATE agent_invocations SET completed_at=?,heartbeat_at=?,progress_at=?,status=?,active=0,model=CASE WHEN ?<>'' THEN ? ELSE model END,
		input_tokens=?,output_tokens=?,cache_read_tokens=?,cost_usd=?,cost_reported=?,estimated_api_cost_usd=?,estimate_complete=?,error=? WHERE id=?`,
		finished, finished, finished, status, model, model, in, out, cached, providerCost, boolToInt(providerReported), estimate, boolToInt(estimatedKnown), errorMessage, id)
	if err != nil {
		return err
	}
	if err = checkAffected(res, nil); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE stage_runs SET
		input_tokens=(SELECT COALESCE(SUM(input_tokens),0) FROM agent_invocations WHERE stage_run_id=stage_runs.id),
		output_tokens=(SELECT COALESCE(SUM(output_tokens),0) FROM agent_invocations WHERE stage_run_id=stage_runs.id),
		cache_read_tokens=(SELECT COALESCE(SUM(cache_read_tokens),0) FROM agent_invocations WHERE stage_run_id=stage_runs.id),
		cost_usd=(SELECT COALESCE(SUM(cost_usd),0) FROM agent_invocations WHERE stage_run_id=stage_runs.id),
		cost_reported=(SELECT COALESCE(MIN(cost_reported),1) FROM agent_invocations WHERE stage_run_id=stage_runs.id),
		estimated_api_cost_usd=CASE WHEN (SELECT COALESCE(MIN(estimate_complete),1) FROM agent_invocations WHERE stage_run_id=stage_runs.id)=1
			THEN (SELECT COALESCE(SUM(estimated_api_cost_usd),0) FROM agent_invocations WHERE stage_run_id=stage_runs.id) ELSE NULL END,
		estimate_complete=(SELECT COALESCE(MIN(estimate_complete),1) FROM agent_invocations WHERE stage_run_id=stage_runs.id),
		model=COALESCE((SELECT model FROM agent_invocations WHERE stage_run_id=stage_runs.id AND model<>'' ORDER BY id DESC LIMIT 1),model),
		process_active=EXISTS(SELECT 1 FROM agent_invocations WHERE stage_run_id=stage_runs.id AND active=1),
		process_id=COALESCE((SELECT MAX(process_id) FROM agent_invocations WHERE stage_run_id=stage_runs.id AND active=1),0)
		WHERE id=(SELECT stage_run_id FROM agent_invocations WHERE id=?)`, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

const invocationCols = `id,stage_run_id,book_id,stage,work_unit,backend,model,process_id,active,heartbeat_at,progress_at,started_at,completed_at,status,input_tokens,output_tokens,cache_read_tokens,cost_usd,cost_reported,estimated_api_cost_usd,estimate_complete,error`

func scanInvocation(sc interface{ Scan(...any) error }) (AgentInvocation, error) {
	var v AgentInvocation
	var active, reported, complete int
	var completed sql.NullString
	var estimate sql.NullFloat64
	err := sc.Scan(&v.ID, &v.StageRunID, &v.BookID, &v.Stage, &v.WorkUnit, &v.Backend, &v.Model, &v.ProcessID, &active, &v.HeartbeatAt, &v.ProgressAt, &v.StartedAt, &completed, &v.Status, &v.InputTokens, &v.OutputTokens, &v.CacheReadTokens, &v.CostUSD, &reported, &estimate, &complete, &v.Error)
	if err != nil {
		return v, err
	}
	v.Active, v.CostReported, v.EstimateComplete = active == 1, reported == 1, complete == 1
	if completed.Valid {
		v.CompletedAt = completed.String
	}
	if estimate.Valid {
		x := estimate.Float64
		v.EstimatedAPICostUSD = &x
	}
	return v, nil
}

func (db *DB) ListAgentInvocations(ctx context.Context, bookID int64) ([]AgentInvocation, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+invocationCols+` FROM agent_invocations WHERE book_id=? ORDER BY id`, bookID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentInvocation
	for rows.Next() {
		v, e := scanInvocation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (db *DB) ActiveAgentInvocations(ctx context.Context) ([]AgentInvocation, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+invocationCols+` FROM agent_invocations WHERE active=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentInvocation
	for rows.Next() {
		v, e := scanInvocation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (db *DB) ActiveAgentInvocationCounts(ctx context.Context) (int, map[int64]int, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT book_id,COUNT(*) FROM agent_invocations WHERE active=1 GROUP BY book_id`)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()
	total, byBook := 0, map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return 0, nil, err
		}
		byBook[id] = n
		total += n
	}
	return total, byBook, rows.Err()
}

func (db *DB) ActiveAgentInvocationCount(ctx context.Context, bookID int64) (int, error) {
	var count int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_invocations WHERE active=1 AND book_id=?`, bookID).Scan(&count)
	return count, err
}
