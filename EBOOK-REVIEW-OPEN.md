# feat/ebook-input - review findings not yet fixed

Two adversarial correctness reviews ran over the branch (chapter derivation,
pipeline stages). Everything that could publish a wrong spoiler position, lose
book text, or hold copyrighted prose has been fixed and committed. What follows
is what was found and deliberately left, with the reasoning - so the PR is
honest about it and none of it is rediscovered from scratch.

Delete this file once the list is empty.

## Open

### 1. `force_audio` cannot be turned back off once it is in the scan cache

`internal/metaops/scan_cache.go` lifts only `hidden` out of a cached scan result
into a read-time overlay, so a `force_audio` override applied after a scan is
reflected, but clearing one is not - the cached candidate keeps the audio
verdict until the folder is rescanned.

Not a correctness bug (the pipeline reads the override, not the cache), but the
Library row lies about which source will be used until a rescan.

**Fix direction:** put `force_audio` in the same read-time overlay as `hidden`.

### 2. `source_path` is not canonicalized in `handleCreateBooks`

`internal/api/handlers_pipeline.go` resolves and persists the path for the ebook
branch, but the general create path stores whatever the client sent. Two spellings
of one folder (a trailing slash, a symlinked root) therefore bypass both the
UNIQUE constraint and the "already enqueued" guard, so one book can be created
twice and processed twice.

**Pre-existing - it affects audio equally and is not introduced by this branch.**
Worth fixing, but it is not an ebook change and does not belong in this PR.

**Fix direction:** run every incoming `source_path` through `metaops.ResolvePath`
in the handler, as the ebook branch now does.

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

### 5. The per-kind artifact lists are four parallel lists

`reclaimable`/`ebookReclaimable` in `internal/scratch`, the purge-invalidated
sentinel list in `internal/scheduler`, and the kind switches in
`internal/pipeline` each encode part of "which artifacts belong to which stage
for which kind". They are consistent today but nothing holds them so, and a
desync runs `fact_pass` over zero chapters.

This is the branch's one real structural fragility. **Fix direction:** one
artifact registry - stage, kind, directory, durable-or-reclaimable - that all
four derive from.

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
