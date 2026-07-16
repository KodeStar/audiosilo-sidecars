// Pure logic for the Library tab: mapping scan results to book candidates,
// filtering, coverage-badge derivation, and series-gap detection. Kept free of
// React so it stays exhaustively unit-testable (components call into it).

import type { BookCandidate, Coverage, ScannedBook } from '@/api/types';

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
// the identity/series fields the daemon needs to enqueue it.
export function toCandidate(book: ScannedBook): BookCandidate {
  return {
    source_path: book.path,
    title: book.title,
    authors: book.authors ?? [],
    series: book.series ?? '',
    series_pos: book.series_position ?? '',
    asin: book.asin ?? '',
    isbn: book.isbn ?? '',
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

// parsePos extracts the leading numeric part of a series position ("1", "2.5",
// "1-3.5" -> 1). Empty/unparseable positions sort last (Infinity), mirroring the
// server's parseSeriesPos so the UI and the scheduler agree on ordering.
export function parsePos(pos: string | undefined): number {
  const s = (pos ?? '').trim();
  if (s === '') return Infinity;
  const m = /^[0-9]*\.?[0-9]+/.exec(s);
  if (!m) return Infinity;
  const n = Number.parseFloat(m[0]);
  return Number.isNaN(n) ? Infinity : n;
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
