-- Total audio duration (seconds) for a book, written by the pipeline right after
-- inspect succeeds (0 = not yet known / pre-inspect / pre-migration). It rides on
-- the book view so the Running list can show each book's length. Additive and
-- defaulted, so rows written before this migration read 0 (the UI hides the chip).
ALTER TABLE books ADD COLUMN duration_sec REAL NOT NULL DEFAULT 0;
