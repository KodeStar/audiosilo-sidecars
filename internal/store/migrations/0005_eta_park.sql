-- ETA engine + typed park reasons (M6). Two additive, defaulted columns on books.
--
-- chapters is the manifest chapter count, written by the pipeline right after
-- inspect succeeds (0 = not yet known). The ETA engine uses it as the per-book
-- chapter total for the per-chapter stages (splitting/asr/sanitizing/
-- retranscribing) instead of the fallback default.
--
-- park_code is the typed park reason that rides beside the free-text error: it is
-- set whenever status becomes needs_attention and cleared whenever status clears
-- (retry/resume/cancel overwrite it like error). Empty = no typed reason. Both are
-- back-compatible with rows written before this migration.
ALTER TABLE books ADD COLUMN chapters INTEGER NOT NULL DEFAULT 0;
ALTER TABLE books ADD COLUMN park_code TEXT NOT NULL DEFAULT '';
