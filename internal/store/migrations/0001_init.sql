-- AudioSilo Sidecars initial schema.
--
-- SQLite is the SCHEDULING truth (which book is at which stage); the work-dir
-- artifacts + _done/<stage>.json sentinels are the CONTENT truth. Startup
-- reconciles the two. Durable auth (password hash, sessions) also lives here so
-- the daemon keeps a single state file.

-- books tracks each selected audiobook through the pipeline. source_path is the
-- identity (a book is enqueued once); work_dir is where its scratch/artifacts
-- live. JSON columns (authors, identity_sources, coverage) hold the shapes the
-- scanner/coverage client produce without reshaping the schema.
CREATE TABLE books (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    source_path       TEXT NOT NULL UNIQUE,
    work_dir          TEXT NOT NULL,
    title             TEXT NOT NULL,
    authors           TEXT NOT NULL DEFAULT '[]',   -- JSON array of names
    series            TEXT NOT NULL DEFAULT '',
    series_pos        TEXT NOT NULL DEFAULT '',
    asin              TEXT NOT NULL DEFAULT '',
    isbn              TEXT NOT NULL DEFAULT '',
    identity_sources  TEXT NOT NULL DEFAULT '{}',   -- JSON provenance map
    state             TEXT NOT NULL DEFAULT 'queued',
    status            TEXT NOT NULL DEFAULT ''
        CHECK (status IN ('', 'paused', 'needs_attention', 'failed')),
    error             TEXT NOT NULL DEFAULT '',
    coverage          TEXT NOT NULL DEFAULT '',      -- JSON coverage snapshot ('' = unknown)
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);
CREATE INDEX idx_books_state ON books(state);

-- stage_runs is the per-execution ledger. An open run (finished_at IS NULL) is
-- an in-flight stage; startup closes any it finds as interrupted (ok=0). A run
-- with ok=1 is the DB's claim that the stage completed (cross-checked against the
-- sentinel on disk during reconcile).
CREATE TABLE stage_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    book_id     INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    stage       TEXT NOT NULL,
    attempt     INTEGER NOT NULL DEFAULT 1,
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    ok          INTEGER,                              -- NULL=running, 0=failed, 1=ok
    metrics     TEXT NOT NULL DEFAULT '{}'            -- JSON
);
CREATE INDEX idx_stage_runs_book ON stage_runs(book_id);

-- progress is the live within-stage counter (chapter i/N, chunk i/N) surfaced in
-- the UI. One row per (book, stage), last-write-wins.
CREATE TABLE progress (
    book_id INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    stage   TEXT NOT NULL,
    done    INTEGER NOT NULL DEFAULT 0,
    total   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (book_id, stage)
);

-- events is the durable log feeding future log views. The SSE hub remains the
-- live fan-out; every published (non-heartbeat) event is also appended here.
-- book_id is NULL for daemon-wide events (e.g. queue stats).
CREATE TABLE events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts      TEXT NOT NULL,
    type    TEXT NOT NULL,
    book_id INTEGER,
    payload TEXT NOT NULL DEFAULT '{}'                -- JSON
);
CREATE INDEX idx_events_ts ON events(ts);

-- rates seeds the EWMA per-stage unit-rate table used by the ETA engine (a later
-- milestone). M1 only creates it.
CREATE TABLE rates (
    stage      TEXT PRIMARY KEY,
    unit_rate  REAL NOT NULL,
    updated_at TEXT NOT NULL
);

-- settings is a small key/value store for durable daemon state that is not
-- config.yaml, including the admin password hash (auth.password_hash).
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- sessions holds live session tokens by SHA-256 hash only (never the raw token),
-- replacing the M0 sessions.json file.
CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    created_at TEXT NOT NULL
);
