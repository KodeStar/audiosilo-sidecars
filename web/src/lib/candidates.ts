// Pure logic for the Library tab: mapping scan results to book candidates,
// filtering, coverage-badge derivation, and series-gap detection. Kept free of
// React so it stays exhaustively unit-testable (components call into it).

import type {
  BookCandidate,
  BookCreateResult,
  Coverage,
  ScannedBook,
  SetOverrideBody,
} from '@/api/types';

// The two expressive-layer dimensions the tool contributes.
export type CoverageDimension = 'characters' | 'recaps';

// The badge state for one dimension:
// - has: the work already carries this sidecar (nothing to contribute)
// - needed: the work exists but lacks this sidecar (a good target)
// - unknown: the book's identity did not resolve to a known work
// - unavailable: the metadata service was disabled or unreachable this scan
export type CoverageState = 'has' | 'needed' | 'unknown' | 'unavailable';

// coverageState derives the per-dimension badge from a Coverage verdict. A
// missing coverage object (older server / not merged) reads as unavailable.
export function coverageState(
  coverage: Coverage | undefined,
  dimension: CoverageDimension,
): CoverageState {
  if (!coverage || !coverage.available) return 'unavailable';
  if (!coverage.known) return 'unknown';
  const has = dimension === 'characters' ? coverage.has_characters : coverage.has_recaps;
  return has ? 'has' : 'needed';
}

// isCovered reports whether a book already has BOTH sidecars, so contributing
// would be redundant. Only a known work with both dimensions present counts.
export function isCovered(book: ScannedBook): boolean {
  const c = book.coverage;
  return !!c && c.available && c.known && c.has_characters && c.has_recaps;
}

// manualWorkId returns the manual-match work id for a book, or '' when the book
// is not manually matched. It is still used for override-payload preservation
// (never clobber a manual match) and provenance display - distinct from the
// enqueue's work_id, which carries any resolved match (see toCandidate).
export function manualWorkId(book: ScannedBook): string {
  const c = book.coverage;
  if (c && c.matched_by === 'manual' && c.work_id) return c.work_id;
  return '';
}

// toCandidate maps a scanned book to the POST /books candidate shape, carrying
// the identity/series fields the daemon needs to enqueue it plus the advisory
// coverage + provenance snapshot from the scan (persisted, not re-derived). The
// resolved work_id is carried through for ANY matched kind (asin/isbn/search/
// manual) so later pipeline/contribution stages reference the matched work via
// books.work_id without re-resolving - there is no server-side re-resolution.
export function toCandidate(book: ScannedBook): BookCandidate {
  const candidate: BookCandidate = {
    source_path: book.source_path,
    title: book.title,
    authors: book.authors ?? [],
    series: book.series ?? '',
    series_pos: book.series_position ?? '',
    asin: book.asin ?? '',
    isbn: book.isbn ?? '',
    coverage: book.coverage,
    sources: book.sources,
  };
  // Carry narrators through so a later core (add-work) proposal can prefill them
  // (metaissue requires >= 1 narrator). Omit an empty list to keep the payload tidy.
  if (book.narrators && book.narrators.length > 0) candidate.narrators = book.narrators;
  const c = book.coverage;
  if (c && c.known && c.work_id) candidate.work_id = c.work_id;
  return candidate;
}

// filterCandidates applies the visible-set filters. Hidden books are dropped
// unless includeHidden is set (the "show hidden" toggle). When excludeCovered is
// true, books that already have both sidecars are dropped. Order is preserved.
export function filterCandidates(
  books: ScannedBook[],
  opts: { excludeCovered: boolean; includeHidden?: boolean },
): ScannedBook[] {
  return books.filter((b) => {
    if (b.hidden && !opts.includeHidden) return false;
    if (opts.excludeCovered && isCovered(b)) return false;
    return true;
  });
}

// hiddenBooks returns just the books the user has hidden (for the "Show hidden
// (n)" affordance and the dimmed hidden section). Order is preserved.
export function hiddenBooks(books: ScannedBook[]): ScannedBook[] {
  return books.filter((b) => b.hidden);
}

// POS_SENTINEL sorts an empty/unparseable position last. It matches the Go
// scheduler.ParseSeriesPos sentinel (1e18) exactly so the two implementations are
// behaviorally identical, not merely both "sort last".
export const POS_SENTINEL = 1e18;

// parsePos is an exact behavioral mirror of the server's scheduler.ParseSeriesPos
// (internal/scheduler/scheduler.go): it takes the leading run of digit/dot
// characters and parses that as a single float, so "1-3.5" -> 1 (stops at '-')
// and "2.5" -> 2.5, while anything the Go strconv.ParseFloat would reject -
// empty, garbage ("bonus"), a bare ".", or a multi-dot run ("1.2.3") - yields
// POS_SENTINEL so it sorts last. Go is the reference; keep them in lockstep.
export function parsePos(pos: string | undefined): number {
  const s = (pos ?? '').trim();
  if (s === '') return POS_SENTINEL;
  // Leading run of [0-9.] only, mirroring Go's byte scan that stops at the first
  // other character.
  const run = /^[0-9.]*/.exec(s)?.[0] ?? '';
  // Go's ParseFloat over that run accepts at most one '.' and needs >= 1 digit;
  // "" / "." / "1.2.3" all fail there and fall through to the sentinel.
  const dots = (run.match(/\./g) ?? []).length;
  if (dots > 1 || !/[0-9]/.test(run)) return POS_SENTINEL;
  const n = Number.parseFloat(run);
  return Number.isNaN(n) ? POS_SENTINEL : n;
}

// ResultTally counts a POST /books response by per-candidate outcome.
export interface ResultTally {
  created: number;
  conflicts: number;
  failed: number;
}

// tallyResults buckets create-book results into created / already-enqueued
// (conflict) / failed. A result is "failed" when it was neither created nor a
// conflict. One helper so the process handler and the summary line agree.
export function tallyResults(results: BookCreateResult[]): ResultTally {
  let created = 0;
  let conflicts = 0;
  let failed = 0;
  for (const r of results) {
    if (r.created) created++;
    else if (r.conflict) conflicts++;
    else failed++;
  }
  return { created, conflicts, failed };
}

// summarizeTally renders a one-line note from an already-computed tally (so the
// tally is computed once per submit and the store + a summary line agree).
export function summarizeTally({ created, conflicts, failed }: ResultTally): string {
  const parts: string[] = [];
  if (created > 0) parts.push(`${created} enqueued`);
  if (conflicts > 0) parts.push(`${conflicts} already enqueued`);
  if (failed > 0) parts.push(`${failed} failed`);
  return parts.length > 0 ? parts.join(', ') + '.' : 'Nothing to enqueue.';
}

// matchProvenanceLabel describes a non-automatic identity match for the UI, or
// null when the match needs no extra chrome (asin/isbn exact matches, no match).
// Only "search" and "manual" surface a label - they are advisory, so the user can
// see (and, for manual, undo) how a book was matched.
export function matchProvenanceLabel(coverage: Coverage | undefined): string | null {
  if (!coverage || !coverage.matched_by) return null;
  const title = coverage.work_title ?? '';
  const suffix = title ? `: ${title}` : '';
  if (coverage.matched_by === 'manual') return `manual match${suffix}`;
  if (coverage.matched_by === 'search') return `matched by title search${suffix}`;
  return null;
}

// isManualMatch reports whether a book carries a user-supplied manual match (so
// the row can offer "Clear match").
export function isManualMatch(coverage: Coverage | undefined): boolean {
  return coverage?.matched_by === 'manual';
}

// clearedCoverage is the local coverage a book reverts to right after its manual
// match is cleared: available but no longer resolved to a work (reads as "unknown
// work"). A fresh scan re-resolves it authoritatively.
export function clearedCoverage(): Coverage {
  return { available: true, known: false, has_characters: false, has_recaps: false };
}

// overridePayload builds the FULL desired-state POST /overrides body for a book,
// preserving whatever the book already carries for the dimension the change does
// not touch: hiding preserves an existing manual work_id (never clobbers a match),
// and matching preserves the current hidden flag. changes.workId === '' clears the
// match; changes.hidden === false unhides.
export function overridePayload(
  book: ScannedBook,
  changes: { hidden?: boolean; workId?: string },
): SetOverrideBody {
  return {
    source_path: book.source_path,
    hidden: changes.hidden ?? book.hidden ?? false,
    work_id: changes.workId ?? manualWorkId(book),
  };
}

// seriesGapHint reports the series for which a selected book skips an earlier
// position that is present in the scan but unselected. Series carryover (the
// "story so far" recaps) works best processed in order, so the UI nudges the
// user. Returns the sorted, de-duplicated list of affected series names.
export function seriesGapHint(books: ScannedBook[], selected: ReadonlySet<string>): string[] {
  const bySeries = new Map<string, ScannedBook[]>();
  for (const b of books) {
    const series = (b.series ?? '').trim();
    if (series === '') continue;
    const arr = bySeries.get(series);
    if (arr) arr.push(b);
    else bySeries.set(series, [b]);
  }

  const affected: string[] = [];
  for (const [series, group] of bySeries) {
    const selectedPositions = group
      .filter((b) => selected.has(b.path))
      .map((b) => parsePos(b.series_position));
    if (selectedPositions.length === 0) continue;
    const minSelected = Math.min(...selectedPositions);
    const hasEarlierUnselected = group.some(
      (b) => !selected.has(b.path) && parsePos(b.series_position) < minSelected,
    );
    if (hasEarlierUnselected) affected.push(series);
  }
  return affected.sort((a, b) => a.localeCompare(b));
}
