-- Library "New" view: when each library folder was FIRST observed by a completed
-- folder scan, so the Library tab can default to the books that appeared since
-- the user last looked instead of hundreds of folders they have already seen.
--
-- Keyed on the canonical absolute source_path like candidate_overrides, with no
-- FK to the rebuildable book index: a sighting is durable user-facing state that
-- must outlive a book being enqueued, completed, deleted or re-scanned.
--
-- baseline=1 marks the sightings recorded by the FIRST batch this daemon ever
-- records (seeded from the cached last scan at startup, else the first completed
-- scan). Those paths are the library as it already stood, so they are never new -
-- without the flag an upgrade would light up every folder the user owns.
--
-- acknowledged_at is the user dismissing a book from the New view ("I have seen
-- this one"); NULL while it is still unacknowledged.
--
-- Timestamps here are plain RFC3339 UTC (seconds precision), NOT the store's
-- fixed-width nanosecond layout: first_seen_at is served verbatim on the wire
-- (ScannedBook.first_seen_at), whose contract pins RFC3339. Every value in this
-- table shares that one layout, so lexicographic compares stay chronological.
CREATE TABLE library_sightings (
    source_path     TEXT PRIMARY KEY,
    first_seen_at   TEXT NOT NULL,
    last_seen_at    TEXT NOT NULL,
    acknowledged_at TEXT,
    baseline        INTEGER NOT NULL DEFAULT 0
);
