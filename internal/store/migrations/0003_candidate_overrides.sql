-- Persistent, path-keyed candidate overrides drive two Library-tab affordances
-- that must survive a daemon restart and a re-scan: a manual "hide this book"
-- and a manual work match (when the fuzzy matcher cannot resolve a book to its
-- meta.audiosilo.app work). source_path is the identity (the scanned book folder),
-- exactly like books.source_path. A row exists only while it says something: the
-- upsert deletes a row that is neither hidden nor manually matched.
CREATE TABLE candidate_overrides (
    source_path TEXT PRIMARY KEY,
    hidden      INTEGER NOT NULL DEFAULT 0,
    work_id     TEXT NOT NULL DEFAULT '',
    work_title  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL
);

-- A book remembers the work it was matched to at enqueue time (from the coverage
-- verdict or a manual match), so the pipeline/contribution stages can reference it
-- without re-resolving. Path stays the identity; work_id is advisory enrichment.
ALTER TABLE books ADD COLUMN work_id TEXT NOT NULL DEFAULT '';
