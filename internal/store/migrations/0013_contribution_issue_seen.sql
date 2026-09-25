-- issue_seen_at is the intake issue's GitHub updated_at when the poller last read
-- the intake bot's verdict comments for this row. A verdict row is re-checked
-- hourly, and its comments are re-read only when the issue has changed since.
ALTER TABLE contributions ADD COLUMN issue_seen_at TEXT NOT NULL DEFAULT '';
