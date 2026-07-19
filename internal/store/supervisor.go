package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// SupervisorRun is one deterministic incident/decision or bounded model invocation.
// It is intentionally separate from StageRun so supervision never alters stage counts.
type SupervisorRun struct {
	ID                        int64           `json:"id"`
	IncidentKey               string          `json:"incident_key,omitempty"`
	BatchID                   string          `json:"batch_id"`
	BookID                    *int64          `json:"book_id,omitempty"`
	StageRunID                *int64          `json:"stage_run_id,omitempty"`
	Trigger                   string          `json:"trigger"`
	Diagnosis                 string          `json:"diagnosis"`
	Confidence                float64         `json:"confidence"`
	Evidence                  json.RawMessage `json:"evidence"`
	Decision                  string          `json:"decision"`
	SelectedAction            string          `json:"selected_action"`
	SuggestedRetryLimit       int             `json:"suggested_retry_limit"`
	SuggestedTerminationLimit int             `json:"suggested_termination_limit"`
	ActionOutcome             string          `json:"action_outcome"`
	Automatic                 bool            `json:"automatic"`
	ApprovalRequired          bool            `json:"approval_required"`
	State                     string          `json:"state"`
	Model                     string          `json:"model,omitempty"`
	Backend                   string          `json:"backend,omitempty"`
	ModelCalls                int             `json:"model_calls"`
	InputTokens               int64           `json:"input_tokens"`
	OutputTokens              int64           `json:"output_tokens"`
	CachedTokens              int64           `json:"cached_tokens"`
	ProviderCostUSD           *float64        `json:"provider_cost_usd,omitempty"`
	ProviderCostComplete      bool            `json:"provider_cost_complete"`
	EstimatedAPICostUSD       *float64        `json:"estimated_api_cost_usd,omitempty"`
	EstimateComplete          bool            `json:"estimate_complete"`
	PricingVersion            string          `json:"pricing_version,omitempty"`
	StartedAt                 string          `json:"started_at"`
	CompletedAt               string          `json:"completed_at,omitempty"`
}

func (db *DB) StartSupervisorRun(ctx context.Context, r SupervisorRun) (int64, error) {
	if r.BatchID == "" {
		r.BatchID = LegacyBatchID
	}
	if len(r.Evidence) == 0 {
		r.Evidence = json.RawMessage(`[]`)
	}
	if r.SelectedAction == "" {
		r.SelectedAction = "observe"
	}
	if r.State == "" {
		r.State = "open"
	}
	if r.StartedAt == "" {
		r.StartedAt = timestamp(nowFn())
	}
	res, err := db.sql.ExecContext(ctx, `INSERT INTO supervisor_runs
		(incident_key,batch_id,book_id,stage_run_id,trigger,diagnosis,confidence,evidence,
		 decision,selected_action,suggested_retry_limit,suggested_termination_limit,action_outcome,automatic,approval_required,state,model,backend,model_calls,
		 input_tokens,output_tokens,cached_tokens,provider_cost_usd,provider_cost_complete,estimated_api_cost_usd,estimate_complete,
		 pricing_version,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.IncidentKey, r.BatchID, nullableInt(r.BookID), nullableInt(r.StageRunID), r.Trigger,
		r.Diagnosis, r.Confidence, string(r.Evidence), r.Decision, r.SelectedAction, r.SuggestedRetryLimit, r.SuggestedTerminationLimit, r.ActionOutcome,
		boolToInt(r.Automatic), boolToInt(r.ApprovalRequired), r.State, r.Model, r.Backend, r.ModelCalls,
		r.InputTokens, r.OutputTokens, r.CachedTokens, nullableFloat(r.ProviderCostUSD), boolToInt(r.ProviderCostComplete),
		nullableFloat(r.EstimatedAPICostUSD), boolToInt(r.EstimateComplete), r.PricingVersion, r.StartedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nullableInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// FinishSupervisorRun closes a run. Usage is overwritten with the complete result so
// failed/model-validation calls remain represented exactly once.
func (db *DB) FinishSupervisorRun(ctx context.Context, r SupervisorRun) error {
	completed := r.CompletedAt
	if completed == "" {
		completed = timestamp(nowFn())
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE supervisor_runs SET
		diagnosis=?,confidence=?,evidence=?,decision=?,selected_action=?,suggested_retry_limit=?,suggested_termination_limit=?,action_outcome=?,
		automatic=?,approval_required=?,state=?,model=?,backend=?,model_calls=?,input_tokens=?,output_tokens=?,
		cached_tokens=?,provider_cost_usd=?,provider_cost_complete=?,estimated_api_cost_usd=?,estimate_complete=?,pricing_version=?,completed_at=?
		WHERE id=?`, r.Diagnosis, r.Confidence, string(r.Evidence), r.Decision, r.SelectedAction,
		r.SuggestedRetryLimit, r.SuggestedTerminationLimit, r.ActionOutcome, boolToInt(r.Automatic), boolToInt(r.ApprovalRequired), r.State, r.Model,
		r.Backend, r.ModelCalls, r.InputTokens, r.OutputTokens, r.CachedTokens, nullableFloat(r.ProviderCostUSD), boolToInt(r.ProviderCostComplete),
		nullableFloat(r.EstimatedAPICostUSD), boolToInt(r.EstimateComplete), r.PricingVersion, completed, r.ID)
	return checkAffected(res, err)
}

const supervisorCols = `id,incident_key,batch_id,book_id,stage_run_id,trigger,diagnosis,
	confidence,evidence,decision,selected_action,suggested_retry_limit,suggested_termination_limit,action_outcome,automatic,approval_required,state,
	model,backend,model_calls,input_tokens,output_tokens,cached_tokens,provider_cost_usd,provider_cost_complete,estimated_api_cost_usd,estimate_complete,
	pricing_version,started_at,completed_at`

func scanSupervisorRun(sc interface{ Scan(...any) error }) (SupervisorRun, error) {
	var r SupervisorRun
	var book, stage sql.NullInt64
	var provider, estimated sql.NullFloat64
	var completed sql.NullString
	var automatic, approval, providerComplete, estimateComplete int64
	var evidence string
	err := sc.Scan(&r.ID, &r.IncidentKey, &r.BatchID, &book, &stage, &r.Trigger, &r.Diagnosis,
		&r.Confidence, &evidence, &r.Decision, &r.SelectedAction, &r.SuggestedRetryLimit, &r.SuggestedTerminationLimit, &r.ActionOutcome, &automatic, &approval,
		&r.State, &r.Model, &r.Backend, &r.ModelCalls, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &provider, &providerComplete, &estimated, &estimateComplete,
		&r.PricingVersion, &r.StartedAt, &completed)
	if err != nil {
		return r, err
	}
	if book.Valid {
		v := book.Int64
		r.BookID = &v
	}
	if stage.Valid {
		v := stage.Int64
		r.StageRunID = &v
	}
	if provider.Valid {
		v := provider.Float64
		r.ProviderCostUSD = &v
	}
	if estimated.Valid {
		v := estimated.Float64
		r.EstimatedAPICostUSD = &v
	}
	if completed.Valid {
		r.CompletedAt = completed.String
	}
	r.Automatic, r.ApprovalRequired = automatic == 1, approval == 1
	r.ProviderCostComplete, r.EstimateComplete = providerComplete == 1, estimateComplete == 1
	r.Evidence = json.RawMessage(evidence)
	return r, nil
}

func (db *DB) RecentSupervisorRuns(ctx context.Context, batchID string, limit int) ([]SupervisorRun, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	q, args := `SELECT `+supervisorCols+` FROM supervisor_runs`, []any{}
	if batchID != "" {
		q += ` WHERE batch_id=?`
		args = append(args, batchID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SupervisorRun
	for rows.Next() {
		r, err := scanSupervisorRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HasIncident reports whether this exact immutable incident event was already
// handled. Incident keys include the relevant stage-run id/fingerprint, so a new
// attempt is a new event while a persistent failed row cannot trigger periodic model
// calls merely because another health tick elapsed.
func (db *DB) HasIncident(ctx context.Context, incidentKey string) (bool, error) {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM supervisor_runs WHERE incident_key=?`, incidentKey).Scan(&n)
	return n > 0, err
}

// CountSupervisorIncidentFamily counts prior decisions for the same underlying
// diagnosis while ignoring the immutable stage-run id embedded in incident_key.
// Production stage attempts are a separate ledger and must never consume the
// supervisor's recovery budget.
func (db *DB) CountSupervisorIncidentFamily(ctx context.Context, kind string, bookID int64, stage, fingerprint string) (int, error) {
	prefix := fmt.Sprintf("%s/%d/%s/", kind, bookID, stage)
	suffix := "/" + fingerprint
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM supervisor_runs
		 WHERE instr(incident_key, ?) = 1
		   AND substr(incident_key, -length(?)) = ?`, prefix, suffix, suffix).Scan(&n)
	return n, err
}

func (db *DB) SupervisorInvocationCountSince(ctx context.Context, since string) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(model_calls),0) FROM supervisor_runs WHERE started_at>=?`, since).Scan(&n)
	return n, err
}

// SupervisorSpend returns budget spend, preferring reported provider cost and falling
// back to the configured estimate. Unknown-cost calls contribute zero here and are
// separately counted so callers can refuse another model call safely.
func (db *DB) SupervisorSpend(ctx context.Context, batchID string, bookID *int64) (cost float64, unknown int, err error) {
	q := `SELECT COALESCE(SUM(CASE
			WHEN provider_cost_complete=1 AND provider_cost_usd IS NOT NULL THEN provider_cost_usd
			WHEN estimate_complete=1 AND estimated_api_cost_usd IS NOT NULL THEN estimated_api_cost_usd
			ELSE COALESCE(provider_cost_usd,estimated_api_cost_usd,0) END),0),
		COALESCE(SUM(CASE WHEN provider_cost_complete=0 AND estimate_complete=0 AND model<>'' THEN 1 ELSE 0 END),0)
		FROM supervisor_runs WHERE batch_id=?`
	args := []any{batchID}
	if bookID != nil {
		q += ` AND book_id=?`
		args = append(args, *bookID)
	}
	err = db.sql.QueryRowContext(ctx, q, args...).Scan(&cost, &unknown)
	return
}

// BatchCostSummary keeps production, book-attributed supervision, and batch-level
// supervision separate. Failed and superseded rows are included by construction.
type BatchCostSummary struct {
	BatchID                        string  `json:"batch_id"`
	ProductionReportedUSD          float64 `json:"production_reported_usd"`
	ProductionReportedIncomplete   bool    `json:"production_reported_incomplete"`
	ProductionEstimatedAPIUSD      float64 `json:"production_estimated_api_usd"`
	ProductionEstimateIncomplete   bool    `json:"production_estimate_incomplete"`
	BookSupervisorReportedUSD      float64 `json:"book_supervisor_reported_usd"`
	BookSupervisorEstimatedAPIUSD  float64 `json:"book_supervisor_estimated_api_usd"`
	BatchSupervisorReportedUSD     float64 `json:"batch_supervisor_reported_usd"`
	BatchSupervisorEstimatedAPIUSD float64 `json:"batch_supervisor_estimated_api_usd"`
	SupervisorReportedIncomplete   bool    `json:"supervisor_reported_incomplete"`
	SupervisorEstimateIncomplete   bool    `json:"supervisor_estimate_incomplete"`
	OverallReportedUSD             float64 `json:"overall_reported_usd"`
	OverallReportedIncomplete      bool    `json:"overall_reported_incomplete"`
	OverallEstimatedAPIUSD         float64 `json:"overall_estimated_api_usd"`
	OverallEstimateIncomplete      bool    `json:"overall_estimate_incomplete"`
}

func (db *DB) BatchCosts(ctx context.Context, batchID string) (BatchCostSummary, error) {
	s := BatchCostSummary{BatchID: batchID}
	var reportedIncomplete, estimateIncomplete int
	err := db.sql.QueryRowContext(ctx, `SELECT COALESCE(SUM(sr.cost_usd),0),
		COALESCE(SUM(COALESCE(sr.estimated_api_cost_usd,0)),0),
		COALESCE(SUM(CASE WHEN sr.cost_reported=0 AND (sr.input_tokens+sr.output_tokens+sr.cache_read_tokens)>0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN sr.estimate_complete=0 AND (sr.input_tokens+sr.output_tokens+sr.cache_read_tokens)>0 THEN 1 ELSE 0 END),0)
		FROM stage_runs sr JOIN books b ON b.id=sr.book_id WHERE b.batch_id=?`, batchID).
		Scan(&s.ProductionReportedUSD, &s.ProductionEstimatedAPIUSD, &reportedIncomplete, &estimateIncomplete)
	if err != nil {
		return s, err
	}
	s.ProductionReportedIncomplete = reportedIncomplete > 0
	s.ProductionEstimateIncomplete = estimateIncomplete > 0
	rows, err := db.sql.QueryContext(ctx, `SELECT book_id IS NULL,
		COALESCE(SUM(COALESCE(provider_cost_usd,0)),0),
		COALESCE(SUM(COALESCE(estimated_api_cost_usd,0)),0),
		COALESCE(SUM(CASE WHEN provider_cost_complete=0 AND model<>'' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN estimate_complete=0 AND model<>'' THEN 1 ELSE 0 END),0)
		FROM supervisor_runs WHERE batch_id=? GROUP BY book_id IS NULL`, batchID)
	if err != nil {
		return s, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var batchLevel int
		var reported, estimated float64
		var reportedUnknown, estimatedUnknown int
		if err := rows.Scan(&batchLevel, &reported, &estimated, &reportedUnknown, &estimatedUnknown); err != nil {
			return s, err
		}
		s.SupervisorReportedIncomplete = s.SupervisorReportedIncomplete || reportedUnknown > 0
		s.SupervisorEstimateIncomplete = s.SupervisorEstimateIncomplete || estimatedUnknown > 0
		if batchLevel == 1 {
			s.BatchSupervisorReportedUSD = reported
			s.BatchSupervisorEstimatedAPIUSD = estimated
		} else {
			s.BookSupervisorReportedUSD = reported
			s.BookSupervisorEstimatedAPIUSD = estimated
		}
	}
	s.OverallReportedUSD = s.ProductionReportedUSD + s.BookSupervisorReportedUSD + s.BatchSupervisorReportedUSD
	s.OverallEstimatedAPIUSD = s.ProductionEstimatedAPIUSD + s.BookSupervisorEstimatedAPIUSD + s.BatchSupervisorEstimatedAPIUSD
	s.OverallReportedIncomplete = s.ProductionReportedIncomplete || s.SupervisorReportedIncomplete
	s.OverallEstimateIncomplete = s.ProductionEstimateIncomplete || s.SupervisorEstimateIncomplete
	return s, rows.Err()
}
