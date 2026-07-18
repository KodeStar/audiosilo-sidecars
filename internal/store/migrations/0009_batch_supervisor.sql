-- Bounded batch supervision. Supervision is an orchestration concern, never a
-- production stage: supervisor_runs has no stage name and is deliberately separate
-- from stage_runs. Existing books are grouped into a durable legacy batch.

CREATE TABLE batches (
    id         TEXT PRIMARY KEY,
    created_at TEXT NOT NULL
);
INSERT INTO batches(id, created_at)
VALUES ('legacy', COALESCE((SELECT MIN(created_at) FROM books), '1970-01-01T00:00:00.000000000Z'));

ALTER TABLE books ADD COLUMN batch_id TEXT NOT NULL DEFAULT 'legacy';
CREATE INDEX idx_books_batch ON books(batch_id);

-- Invocation liveness and explicit cost semantics. cost_usd remains the compatible
-- provider-reported total. A zero value is not assumed to mean free: cost_reported
-- states whether the provider supplied a cost, while estimated_api_cost_usd is NULL
-- unless the configured, versioned pricing table could price every invocation.
ALTER TABLE stage_runs ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stage_runs ADD COLUMN cost_reported INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stage_runs ADD COLUMN estimated_api_cost_usd REAL;
ALTER TABLE stage_runs ADD COLUMN estimate_complete INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stage_runs ADD COLUMN heartbeat_at TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_runs ADD COLUMN progress_at TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_runs ADD COLUMN process_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stage_runs ADD COLUMN process_active INTEGER NOT NULL DEFAULT 0;

UPDATE stage_runs
SET cost_reported = CASE WHEN cost_usd > 0 THEN 1 ELSE 0 END,
    heartbeat_at = started_at,
    progress_at = started_at;

CREATE TABLE supervisor_runs (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    incident_key             TEXT NOT NULL DEFAULT '',
    batch_id                 TEXT NOT NULL REFERENCES batches(id),
    book_id                  INTEGER REFERENCES books(id) ON DELETE SET NULL,
    stage_run_id             INTEGER REFERENCES stage_runs(id) ON DELETE SET NULL,
    trigger                  TEXT NOT NULL,
    diagnosis                TEXT NOT NULL DEFAULT '',
    confidence               REAL NOT NULL DEFAULT 1,
    evidence                 TEXT NOT NULL DEFAULT '[]',
    decision                 TEXT NOT NULL DEFAULT '',
    selected_action          TEXT NOT NULL DEFAULT 'observe',
    suggested_retry_limit    INTEGER NOT NULL DEFAULT 0,
    suggested_termination_limit INTEGER NOT NULL DEFAULT 0,
    action_outcome           TEXT NOT NULL DEFAULT '',
    automatic                INTEGER NOT NULL DEFAULT 0,
    approval_required        INTEGER NOT NULL DEFAULT 0,
    state                    TEXT NOT NULL DEFAULT 'open',
    model                    TEXT NOT NULL DEFAULT '',
    backend                  TEXT NOT NULL DEFAULT '',
    model_calls              INTEGER NOT NULL DEFAULT 0,
    input_tokens             INTEGER NOT NULL DEFAULT 0,
    output_tokens            INTEGER NOT NULL DEFAULT 0,
    cached_tokens            INTEGER NOT NULL DEFAULT 0,
    provider_cost_usd        REAL,
    provider_cost_complete   INTEGER NOT NULL DEFAULT 0,
    estimated_api_cost_usd   REAL,
    estimate_complete        INTEGER NOT NULL DEFAULT 0,
    pricing_version          TEXT NOT NULL DEFAULT '',
    started_at               TEXT NOT NULL,
    completed_at             TEXT
);
CREATE INDEX idx_supervisor_runs_batch ON supervisor_runs(batch_id, id DESC);
CREATE INDEX idx_supervisor_runs_book ON supervisor_runs(book_id, id DESC);
CREATE INDEX idx_supervisor_runs_incident ON supervisor_runs(incident_key, id DESC);
