-- Timed agent self-resume + durable cost history. Two additive, defaulted columns.
--
-- books.retry_at: an RFC3339 (UTC) instant at which the scheduler may automatically
-- re-admit a book that parked on a transient agent condition (agent_unavailable /
-- agent_rate_limited), so an overnight batch heals itself when the CLI comes back or
-- the rate-limit window elapses instead of stranding until a human clicks Retry. ''
-- means "no scheduled retry" (a plain park, or a book that predates this migration -
-- such a book NEVER auto-readmits, only a human Retry re-admits it). It is set only
-- alongside a needs_attention status and cleared whenever status clears (retry/
-- resume/cancel), enforced at the store write like park_code.
--
-- stage_runs.superseded: 1 marks an ok=1 run whose SUCCESS no longer counts for
-- scheduling (round/fix-loop counters, crash-resume "stage done" set) because a Retry
-- reset that stage - but whose recorded COST is still real spend. Replaces the old
-- DELETE-on-retry (which destroyed a book's recorded adjudication/fix cost). Round
-- counters read `ok=1 AND superseded=0`; cost aggregates (SumStageRunCost /
-- StageRunTotals / the Done-tab per-stage table) read ALL rows - spend is spend.
ALTER TABLE books ADD COLUMN retry_at TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_runs ADD COLUMN superseded INTEGER NOT NULL DEFAULT 0;
