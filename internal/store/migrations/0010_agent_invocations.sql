-- Durable per-agent-invocation ledger. stage_runs remains the compatible stage
-- summary; these rows are the source of truth for child process liveness and
-- invocation-level spend. Failed/cancelled/retried invocations are never removed.
CREATE TABLE agent_invocations (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    stage_run_id             INTEGER NOT NULL REFERENCES stage_runs(id) ON DELETE CASCADE,
    book_id                  INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    stage                    TEXT NOT NULL,
    work_unit                TEXT NOT NULL,
    backend                  TEXT NOT NULL DEFAULT '',
    model                    TEXT NOT NULL DEFAULT '',
    process_id               INTEGER NOT NULL DEFAULT 0,
    active                   INTEGER NOT NULL DEFAULT 1,
    heartbeat_at             TEXT NOT NULL,
    progress_at              TEXT NOT NULL,
    started_at               TEXT NOT NULL,
    completed_at             TEXT,
    status                   TEXT NOT NULL DEFAULT 'running'
        CHECK (status IN ('running','success','validation_failed','failure','cancelled')),
    input_tokens             INTEGER NOT NULL DEFAULT 0,
    output_tokens            INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens        INTEGER NOT NULL DEFAULT 0,
    cost_usd                 REAL NOT NULL DEFAULT 0,
    cost_reported            INTEGER NOT NULL DEFAULT 0,
    estimated_api_cost_usd   REAL,
    estimate_complete        INTEGER NOT NULL DEFAULT 0,
    error                    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_agent_invocations_stage_run ON agent_invocations(stage_run_id, id);
CREATE INDEX idx_agent_invocations_active ON agent_invocations(active, book_id);
