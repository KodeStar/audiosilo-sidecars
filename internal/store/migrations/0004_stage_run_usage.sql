-- Per-execution agent cost/usage capture (M5). Every agent invocation accumulates
-- token counts (and a USD cost when the backend reports one) onto the OPEN stage_run
-- for its (book, stage), so a crash preserves spend already incurred. Mechanical
-- and ASR stages leave these at their zero defaults. model records the last model
-- used for the run (agent stages only). All additive, defaulted, and back-compatible
-- with rows written before this migration.
ALTER TABLE stage_runs ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_runs ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stage_runs ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stage_runs ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0;
