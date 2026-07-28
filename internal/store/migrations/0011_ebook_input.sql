-- Ebook input: which front half of the pipeline a book runs, and the epub that
-- drives it. Additive and defaulted, so every pre-migration row reads as the
-- audio pipeline it already is.

-- 'audio' runs inspect -> split -> ASR -> QA -> spelling before the authoring
-- tail; 'ebook' runs extracting over ebook_path and skips all of it, because an
-- epub's text is already exact.
ALTER TABLE books ADD COLUMN kind TEXT NOT NULL DEFAULT 'audio';

-- The DRM-free .epub the pipeline reads. Empty for an audio book.
--
-- For an epub sitting BESIDE an audiobook, source_path stays the audiobook
-- folder while ebook_path names the epub inside it. That split is deliberate:
-- source_path is the durable identity every other table, the Library tab and
-- POST /books key on, and the audiobook's tags carry the ASIN, which is by far
-- the strongest coverage-match key. The epub supplies the text; the audiobook
-- keeps supplying the identity.
--
-- For an ebook-only book the two are the same .epub path.
ALTER TABLE books ADD COLUMN ebook_path TEXT NOT NULL DEFAULT '';

-- Word count of the extracted chapter universe (0 = unknown / audio). An ebook
-- has no runtime, so duration_sec stays 0 and the Running tab shows this
-- instead; without it an ebook row carries no size signal at all.
ALTER TABLE books ADD COLUMN words INTEGER NOT NULL DEFAULT 0;

-- Forces the audio pipeline for a candidate whose folder also holds an epub (a
-- wrong edition, an abridgement, a bad conversion). Keyed like every other
-- override on the canonical absolute source_path.
ALTER TABLE candidate_overrides ADD COLUMN force_audio INTEGER NOT NULL DEFAULT 0;
