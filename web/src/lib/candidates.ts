// Pure logic for the Library tab: mapping scan results to book candidates,
// filtering, coverage-badge derivation, and series-gap detection. Kept free of
// React so it stays exhaustively unit-testable (components call into it).

import type { BookCandidate, BookCreateResult, Coverage, ScannedBook } from '@/api/types';

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

// toCandidate maps a scanned book to the POST /books candidate shape, carrying
// the identity/series fields the daemon needs to enqueue it plus the advisory
// coverage + provenance snapshot from the scan (persisted, not re-derived).
export function toCandidate(book: ScannedBook): BookCandidate {
  return {
    source_path: book.path,
    title: book.title,
    authors: book.authors ?? [],
    series: book.series ?? '',
    series_pos: book.series_position ?? '',
    asin: book.asin ?? '',
    isbn: book.isbn ?? '',
    coverage: book.coverage,
    sources: book.sources,
  };
}

// filterCandidates applies the visible-set filters. When excludeCovered is true,
// books that already have both sidecars are hidden. Order is preserved.
export function filterCandidates(
  books: ScannedBook[],
  opts: { excludeCovered: boolean },
): ScannedBook[] {
  if (!opts.excludeCovered) return books;
  return books.filter((b) => !isCovered(b));
}

// POS_SENTINEL sorts an empty/unparseable position last. It matches the Go
// parseSeriesPos sentinel (1e18) exactly so the two implementations are
// behaviorally identical, not merely both "sort last".
export const POS_SENTINEL = 1e18;

// parsePos is an exact behavioral mirror of the server's parseSeriesPos
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
