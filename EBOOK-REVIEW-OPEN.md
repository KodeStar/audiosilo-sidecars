# feat/ebook-input - review findings not yet fixed

Two adversarial correctness reviews ran over the branch (chapter derivation,
pipeline stages). Everything that could publish a wrong spoiler position, lose
book text, or hold copyrighted prose has been fixed and committed. What follows
is what was found and deliberately left, with the reasoning - so the PR is
honest about it and none of it is rediscovered from scratch.

Delete this file once the list is empty.

## Open

### 1. `force_audio` cannot be turned back off once it is in the scan cache

`force_audio` is already in the read-time overlay. The problem is its
REPRESENTATION: unlike `hidden`, it is not a field on `ScannedBook` - it is
encoded by erasing `Kind`, which is lossy. So the overlay can apply
`forceAudio == true` but cannot undo it once `annotateEbook` baked `Kind: ""`
into the cached base at scan time.

The client half has the same shape: `currentForceAudio` reconstructs a
persisted server boolean from the absence of three fields. That is exact today
only because nothing else sets `EbookPath` without `Kind` - a coincidence, not
an invariant.

**Fix direction:** add `force_audio bool` to `ScannedBook` and set it from the
override, exactly as `hidden` is; then `currentForceAudio` is `!!book.force_audio`
and the cache case closes with it. Note `Override` already carries the field on
the wire - it is missing only from the object the UI renders.

### 2. Path canonicalization is not applied at every metaops boundary

FIXED for the API: `metaops.AllowedPath` now resolves once and returns the
canonical path, and both `handleCreateBooks` call sites persist it.

Still open: `OverrideService.Upsert` treats a resolve failure as "keep the raw
string" while the API treats it as "not allowed" - two policies for one failure.
Worth unifying on the next pass through `metaops`.

### 3. Two sources of truth for "is this an ebook"

`classifyBookEdges` dispatches on the MANIFEST's `Style`, while `chapterTextDir`,
`chunkPlanAuthor`, `factPassChunk` and `ngramCheck` dispatch on `book.Kind`.
Nothing asserts the two agree. A `kind=ebook` row over a `style=files` manifest
would route half the authoring tail down each path - audio edge classification
over an ebook text layer, which is the off-by-one shift `classifyEbookEdges`
exists to prevent.

Unreachable today: only `extracting` writes a `style=ebook` manifest, and only an
ebook book runs it. But it is unguarded, and the failure is silent.

**Fix direction:** assert the manifest style against `book.Kind` once, where the
manifest is read, and fail loudly on a mismatch.

### 4. A crash between the chapter text and the sentinel pays another agent round

`chaptermap_stage.go` gates the free deterministic reparse on the draft being
non-contiguous. The agent branch writes the final contiguous map back to
`extract_manifest.json` BEFORE writing the sentinel, so a crash in that window
leaves a draft that is now contiguous - the gate is skipped, the agent runs a
second time, and if that round declines, the book parks despite correct output
already being on disk.

Costs money and can park a completed book; it cannot publish a wrong number
(whatever the second round returns is validated the same way).

**Fix direction:** write the sentinel before persisting the map, or record the
harvest separately from the derived manifest.

### 5. The remaining per-kind switches in `internal/pipeline`

The load-bearing half of this is FIXED: `internal/scratch` now holds one
`{dir, stage}` artifact table, `Purge` folds over the dirs and
`scratch.InvalidatedStages` derives the sentinel list from the same rows, so the
reclaim set and the invalidation set cannot drift. (They already had: the ebook
list omitted `chapter_mapping`, which wedged any book that needed the mapping
agent - a third of the validation corpus - if it was ever purged.)

Still open, and lower risk: `chapterTextDir` / `chunkPlanAuthor` / `isEbook` in
`internal/pipeline` are per-kind NAMING rather than a safety coupling. Folding
them into the same table would be tidier but nothing silently breaks if they
drift - the stage fails loudly on a missing file.

Also noted: `classifyBookEdges` dispatches on the manifest's `Style` while the
rest of the tail dispatches on `book.Kind` (item 3 above), which is the one
remaining place two sources of truth could disagree.

## Fixed on the branch (for reference)

Chapter derivation (`69a2f64`): strict-beats-a-longer-loose-run; ordinal
numbering of unlabeled continuations (86 chapters derived for a 26-section real
book); the last chapter's tail discarded as back matter; a labeled interstitial
folded backward; a 0-based book losing its prologue; apparatus numbered as story;
no way to tell a divider page from a chapter (Romeo and Juliet, The Adventures of
Sherlock Holmes both published straight to `fact_pass` with every position
shifted); head leaking prose for space-less scripts; macOS AppleDouble stubs
suppressing every real epub; a cancelled metadata pass returning a silently
truncated candidate set.

Pipeline (`5acff40`): facts reused across a re-derived chapter numbering; an
ebook chunk staged with no prose in it; `classifyEbookEdges` reporting a logical
count of 0; the reparse leaving the extract manifest contradicting what shipped.

Scheduler (`de83f66`, `6c11bed`): ebook source prose retained indefinitely when
`contribution.auto_purge` was off, invisible to the scratch gauge, and
unreleasable while parked for a human.

UI (`29977ca`): the whole ebook front half was unreachable - `BookCandidate`
carried no kind, so the Library enqueued every epub as an audio book.
