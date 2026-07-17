# Ebook input (design proposal)

> Status: **proposal, not yet implemented.** A future milestone (call it "M9").
> This documents the shape of adding EPUB input to the pipeline so the work can
> be scoped and spiked before it is built. Hyphens, never em dashes (workspace
> rule).

## The idea

Let the tool accept an **ebook (EPUB)** as an alternative input to an audiobook
folder. When the source is text, the pipeline can **skip the entire audio
front-end** (inspect / split / ASR / transcript-sanitize) **and the whole QA +
spelling-correction machinery**, because there is no ASR degeneration to detect
and no mis-heard proper nouns to correct - the book text already carries the
exact spellings and clean prose. The result is a much shorter, cheaper, faster
path to the same characters/recaps sidecars.

## Why this is tractable (most of it already exists)

The current audio pipeline is the *audio variant* of a process whose **original
form was epub-based**. The building blocks are already shipped:

- **audiosilo-meta `pkg/extract`** (a PUBLIC dep this module already consumes for
  the n-gram check):
  - `Split(epubPath, outDir) (*Manifest, error)` - mechanical epub -> one
    plain-text file per spine doc + `manifest.json` (toc label, conservatively
    inferred chapter number, word counts, warnings for unusable tocs / merged
    chapters).
  - `NGram(source, sidecars, n) ([]Finding, error)` - the no-verbatim overlap
    check, already used by the `validating` stage.
- **audiosilo-meta EXTRACTION.md** documents the exact epub agent process:
  `split -> rolling fact pass -> notes-only synthesis -> spoiler audit`. That is
  **identical to the back half of this pipeline** (`fact_pass -> synthesizing ->
  auditing -> fixing -> validating -> contributing`).
- The **agent runner, staged-context invariants, cost capture, contribution
  machinery, scan/coverage, Running/Done boards** are all source-agnostic
  already.

So ebook support is a **second input modality feeding the same back half**, not a
new pipeline.

## Stage map: audio vs ebook

```
AUDIO (today):
  inspecting -> splitting -> asr -> sanitizing -> qa_sweep -> qa_adjudicating
    -> [retranscribing loop] -> spelling_research -> correcting
    -> markers_normalizing -> fact_pass -> synthesizing -> auditing
    -> [fixing loop] -> validating -> contributing

EBOOK (proposed):
  extracting -> fact_pass -> synthesizing -> auditing -> [fixing loop]
    -> validating -> contributing
```

The ebook path **drops ~9 stages** (inspecting, splitting, asr, sanitizing,
qa_sweep, qa_adjudicating, retranscribing, spelling_research, correcting,
markers_normalizing) and **adds one** (`extracting`, wrapping
`pkg/extract.Split`). No ASR compute at all; ~4 agent stages instead of ~7.

## What changes, by area

1. **Book "source kind"** - a new durable field `books.kind` in {audio, ebook}
   (a migration; default `audio` for back-compat). Set at enqueue time from what
   the user pointed at.

2. **State machine (`internal/state`)** - the table is already the source of
   truth for lanes/transitions. Two clean options: a per-kind entry point +
   successor table (audio starts at `inspecting`, ebook at `extracting`, whose
   mainline successor is `fact_pass`); or a second small transition table
   selected by kind. The state package is pure and table-driven, so this is
   additive. The ETA engine (`MainlineNext`) and the web timeline hand-mirror
   both consume the table, so they follow automatically (the timeline drift-guard
   test needs a per-kind expectation).

3. **`extracting` stage (`internal/pipeline`, new mechanical stage)** - wraps
   `pkg/extract.Split(epub, workDir/extract)`, writes a manifest that feeds the
   position model (chapter numbers), and **quarantines front/back matter and
   teaser excerpts** (EXTRACTION.md's Killing-Floor "Die Trying excerpt" trap -
   an excerpt of another book must never reach the fact pass). Where the manifest
   is ambiguous (no usable toc labels, one file holding many chapters), **park
   `needs_attention` with a typed park code** (`extract_ambiguous`) exactly like
   the audio `markers_normalizing` park - a human maps chapters, then retries.

4. **`fact_pass` prompt** - reuse the stage, but the ebook variant can be **less
   defensive about proper-noun spelling** (they are exact from the text) and
   skips the "trust the ASR corrections sheet" carryover. The spoiler / own-words
   / chapter-attribution rules are unchanged. Prompts are per-stage templates
   already; add an ebook variant or a conditional block.

5. **Scratch / source handling** - same rule as audio: **the epub text never
   enters the repo**; only derived sidecars leave. `extracting` output (chapter
   text) is scratch, auto-purged after `done`. The `validating` stage's n-gram
   check runs against the extracted text (already the contract).

6. **Library / scan (`internal/metaops` + web)** - accept an **epub file or a
   folder of epubs**. Coverage identity by **ISBN (epubs usually carry one) ->
   title-search fallback** (asin is audio-centric; the existing asin->isbn->title
   cascade already covers this). The scan candidate list gets a kind badge. This
   is the fuzziest part (epub metadata quality varies) and worth a spike.

7. **Duration / ETA** - ebooks have no runtime; the Running-list duration chip
   should show word-count or chapter-count for ebooks instead, and the ETA seeds
   differ (no ASR seconds; agent-stage rates dominate).

## Risks / open questions (for the spike)

- **Chapter/position mapping** is the crux. `Split`'s conservative inference
  leaves many docs unnumbered; the audio path had markers, the ebook path has toc
  labels. Expect a manual-mapping park for a meaningful fraction of books. The
  manifest-review step in EXTRACTION.md is currently a human step - the tool needs
  a UI for it (a "map chapters" affordance), which is net-new UX.
- **Front/back-matter and cross-book excerpt detection** must be robust (spoiler +
  wrong-book hazard). Start conservative: quarantine anything unnumbered at the
  edges and surface it for confirmation.
- **DRM.** Only DRM-free epubs are in scope (this tool is a contributor aid, not a
  de-DRM tool; that stays in the manager's Audible flow). Document the boundary.
- **Format scope.** EPUB first (`Split` handles it). MOBI/AZW/PDF are out of scope
  initially (PDF has no clean chapter model).
- **Identity from epub metadata** is less reliable than an Audible ASIN; the
  manual-match modal (already built) is the escape hatch.

## Rough sizing

A focused milestone: one migration + a `kind` field, a per-kind state path, one
new mechanical stage over an existing library call, an ebook `fact_pass` prompt
variant, scan/coverage acceptance of epubs, and the chapter-mapping UI (the
largest single piece). The back half (synthesis / audit / validate / contribute)
and all cost / observability / contribution plumbing are reused unchanged. The
chapter-mapping UX and epub-identity quality are where the real work and the
unknowns are.

## Recommendation

Worth doing - it materially widens what the tool can contribute (many books exist
as ebooks but not as owned audiobooks) and reuses roughly two-thirds of the
machine. Suggested next step is a **short spike** on (a) `Split` chapter-mapping
quality across a handful of real epubs and (b) epub-metadata identity / coverage
hit-rate, then scope the milestone around whatever chapter-mapping UI the spike
shows is necessary.
