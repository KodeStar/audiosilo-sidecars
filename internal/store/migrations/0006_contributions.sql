-- Contribution tracking (M7). A book's validated sidecars are contributed to
-- KodeStar/audiosilo-meta as intake issues, a direct PR, or a local export; this
-- table records the live status of each contributed artifact so the Done board can
-- show issue -> pr -> merged and the poller can advance open rows.
--
-- One row per (book_id, kind): kind is characters/recaps (the two sidecars) or core
-- (an add-work proposal when the work does not yet exist upstream). The unique index
-- makes UpsertContribution idempotent, so a crash between submit and sentinel never
-- double-posts. number/url identify the created issue (issue mode) or PR (pr mode);
-- pr_number/pr_url track the intake bot PR that an issue-mode contribution produces.
-- All columns are additive with safe defaults.
CREATE TABLE contributions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    book_id    INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('characters', 'recaps', 'core')),
    mode       TEXT NOT NULL CHECK (mode IN ('issue', 'pr', 'local')),
    repo       TEXT NOT NULL DEFAULT '',
    number     INTEGER NOT NULL DEFAULT 0,   -- issue number (issue mode) or PR number (pr mode)
    url        TEXT NOT NULL DEFAULT '',
    pr_number  INTEGER NOT NULL DEFAULT 0,   -- intake bot PR number (issue mode)
    pr_url     TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL CHECK (status IN
        ('submitted', 'pr_open', 'merged', 'closed', 'local', 'already_covered')),
    note       TEXT NOT NULL DEFAULT '',     -- e.g. "labels missing - needs maintainer label"
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX contributions_book_kind ON contributions(book_id, kind);

-- narrators mirrors the authors JSON-array column: the scan's narrator credits, kept
-- so the contributing stage can compose a core (add-work) proposal (metaissue requires
-- at least one narrator). Additive, defaulted to an empty array, back-compatible.
ALTER TABLE books ADD COLUMN narrators TEXT NOT NULL DEFAULT '[]';
