-- Account each book's on-disk scratch (chapters + durables) as a persisted
-- column instead of walking the work dir on every read. The split stage writes
-- it at completion (one DirSize walk); PurgeScratch recomputes what remains; row
-- deletion (ON DELETE CASCADE) reclaims it on delete. The book list and the
-- daemon-total gauge on /system are then served from the column with no walk.
ALTER TABLE books ADD COLUMN scratch_bytes INTEGER NOT NULL DEFAULT 0;
