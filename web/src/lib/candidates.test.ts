import { describe, it, expect } from 'vitest';
import type { BookCreateResult, Coverage, ScannedBook } from '@/api/types';
import {
  clearedCoverage,
  coverageState,
  filterCandidates,
  hiddenBooks,
  isCovered,
  isManualMatch,
  manualWorkId,
  matchProvenanceLabel,
  overridePayload,
  parsePos,
  POS_SENTINEL,
  searchCandidates,
  seriesGapHint,
  sortBySeries,
  summarizeTally,
  tallyResults,
  toCandidate,
} from './candidates';

function cov(partial: Partial<Coverage>): Coverage {
  return {
    available: true,
    known: true,
    has_characters: false,
    has_recaps: false,
    ...partial,
  };
}

function book(partial: Partial<ScannedBook>): ScannedBook {
  return {
    path: partial.path ?? '/x',
    source_path: partial.source_path ?? '/root' + (partial.path ?? '/x'),
    title: partial.title ?? 'T',
    audio_files: partial.audio_files ?? 1,
    coverage: partial.coverage ?? cov({}),
    ...partial,
  };
}

describe('coverageState', () => {
  it('reports unavailable when coverage is missing or the service was down', () => {
    expect(coverageState(undefined, 'characters')).toBe('unavailable');
    expect(coverageState(cov({ available: false }), 'recaps')).toBe('unavailable');
  });

  it('reports unknown when available but no work matched', () => {
    expect(coverageState(cov({ available: true, known: false }), 'characters')).toBe('unknown');
  });

  it('distinguishes has vs needed per dimension', () => {
    const c = cov({ known: true, has_characters: true, has_recaps: false });
    expect(coverageState(c, 'characters')).toBe('has');
    expect(coverageState(c, 'recaps')).toBe('needed');
  });
});

describe('isCovered', () => {
  it('is true only when a known work has both sidecars', () => {
    expect(isCovered(book({ coverage: cov({ has_characters: true, has_recaps: true }) }))).toBe(
      true,
    );
    expect(isCovered(book({ coverage: cov({ has_characters: true, has_recaps: false }) }))).toBe(
      false,
    );
    expect(isCovered(book({ coverage: cov({ known: false }) }))).toBe(false);
    expect(isCovered(book({ coverage: cov({ available: false }) }))).toBe(false);
  });
});

describe('toCandidate', () => {
  it('maps scan fields to the POST /books candidate shape, carrying coverage + provenance', () => {
    const coverage = cov({ known: true, has_characters: true });
    const sources = { title: 'tag', series: 'path' };
    const c = toCandidate(
      book({
        path: '/lib/b1',
        title: 'Book One',
        authors: ['Jane Roe'],
        series: 'Saga',
        series_position: '2',
        asin: 'B001',
        coverage,
        sources,
      }),
    );
    expect(c).toEqual({
      source_path: '/root/lib/b1',
      title: 'Book One',
      authors: ['Jane Roe'],
      series: 'Saga',
      series_pos: '2',
      asin: 'B001',
      isbn: '',
      coverage,
      sources,
    });
    // Known work but no work_id on the coverage -> no work_id key at all.
    expect('work_id' in c).toBe(false);
  });

  it('carries the resolved work_id for any matched kind', () => {
    const manual = toCandidate(
      book({ path: '/m', coverage: cov({ matched_by: 'manual', work_id: 'w42' }) }),
    );
    expect(manual.work_id).toBe('w42');

    // An automatic asin/isbn/search match also carries its work_id (books.work_id
    // is set once at enqueue; nothing re-resolves it later).
    const auto = toCandidate(
      book({ path: '/a', coverage: cov({ matched_by: 'asin', work_id: 'w7' }) }),
    );
    expect(auto.work_id).toBe('w7');

    // An unresolved (unknown) book carries no work_id.
    const unknown = toCandidate(
      book({ path: '/u', coverage: cov({ known: false, work_id: 'wX' }) }),
    );
    expect('work_id' in unknown).toBe(false);
  });

  it('uses authoritative series metadata from a known match', () => {
    const candidate = toCandidate(
      book({
        series: 'Incorrect Local Series',
        series_position: '99',
        coverage: cov({
          matched_by: 'manual',
          work_id: 'w-series',
          series: { name: 'Matched Saga', position: '3' },
        }),
      }),
    );

    expect(candidate.series).toBe('Matched Saga');
    expect(candidate.series_pos).toBe('3');
    expect(candidate.sources?.series).toBe('metadata');
    expect(candidate.sources?.series_position).toBe('metadata');
  });

  it('carries narrators through when the scan found them, else omits the key', () => {
    const withNarrators = toCandidate(
      book({ path: '/n', narrators: ['Nora Narrator', 'Sam Speaker'] }),
    );
    expect(withNarrators.narrators).toEqual(['Nora Narrator', 'Sam Speaker']);

    // No narrators -> the key is omitted (kept tidy, like an empty work_id).
    const none = toCandidate(book({ path: '/nn' }));
    expect('narrators' in none).toBe(false);

    const empty = toCandidate(book({ path: '/ne', narrators: [] }));
    expect('narrators' in empty).toBe(false);
  });
});

describe('filterCandidates', () => {
  const covered = book({ path: '/a', coverage: cov({ has_characters: true, has_recaps: true }) });
  const partial = book({ path: '/b', coverage: cov({ has_characters: true }) });

  it('passes everything through when not excluding', () => {
    expect(filterCandidates([covered, partial], { excludeCovered: false })).toHaveLength(2);
  });

  it('hides fully-covered books when excluding', () => {
    const out = filterCandidates([covered, partial], { excludeCovered: true });
    expect(out).toEqual([partial]);
  });

  it('drops hidden books by default and reveals them with includeHidden', () => {
    const shown = book({ path: '/s' });
    const gone = book({ path: '/h', hidden: true });
    expect(filterCandidates([shown, gone], { excludeCovered: false })).toEqual([shown]);
    expect(filterCandidates([shown, gone], { excludeCovered: false, includeHidden: true })).toEqual(
      [shown, gone],
    );
  });

  it('applies excludeCovered and hidden together', () => {
    const hiddenCovered = book({
      path: '/hc',
      hidden: true,
      coverage: cov({ has_characters: true, has_recaps: true }),
    });
    expect(filterCandidates([covered, partial, hiddenCovered], { excludeCovered: true })).toEqual([
      partial,
    ]);
  });
});

describe('hiddenBooks', () => {
  it('returns only the hidden books, order preserved', () => {
    const a = book({ path: '/a' });
    const b = book({ path: '/b', hidden: true });
    const c = book({ path: '/c', hidden: true });
    expect(hiddenBooks([a, b, c])).toEqual([b, c]);
  });
});

describe('manualWorkId / isManualMatch', () => {
  it('reports the work id only for a manual match with a work_id', () => {
    expect(manualWorkId(book({ coverage: cov({ matched_by: 'manual', work_id: 'w1' }) }))).toBe(
      'w1',
    );
    expect(manualWorkId(book({ coverage: cov({ matched_by: 'asin', work_id: 'w1' }) }))).toBe('');
    expect(manualWorkId(book({ coverage: cov({ matched_by: 'manual' }) }))).toBe('');
    expect(manualWorkId(book({}))).toBe('');
  });

  it('isManualMatch tracks the matched_by discriminator', () => {
    expect(isManualMatch(cov({ matched_by: 'manual' }))).toBe(true);
    expect(isManualMatch(cov({ matched_by: 'search' }))).toBe(false);
    expect(isManualMatch(undefined)).toBe(false);
  });
});

describe('matchProvenanceLabel', () => {
  it('labels search and manual matches, with the work title when present', () => {
    expect(matchProvenanceLabel(cov({ matched_by: 'manual', work_title: 'Dune' }))).toBe(
      'manual match: Dune',
    );
    expect(matchProvenanceLabel(cov({ matched_by: 'search', work_title: 'Dune' }))).toBe(
      'matched by title search: Dune',
    );
    expect(matchProvenanceLabel(cov({ matched_by: 'manual' }))).toBe('manual match');
  });

  it('returns null for automatic exact matches and no match', () => {
    expect(matchProvenanceLabel(cov({ matched_by: 'asin' }))).toBeNull();
    expect(matchProvenanceLabel(cov({ matched_by: 'isbn' }))).toBeNull();
    expect(matchProvenanceLabel(cov({}))).toBeNull();
    expect(matchProvenanceLabel(undefined)).toBeNull();
  });
});

describe('clearedCoverage', () => {
  it('is available but unknown (reads as "unknown work")', () => {
    expect(clearedCoverage()).toEqual({
      available: true,
      known: false,
      has_characters: false,
      has_recaps: false,
    });
  });
});

describe('overridePayload', () => {
  it('hides while preserving an existing manual work id (never clobbers a match)', () => {
    const b = book({ path: '/b', coverage: cov({ matched_by: 'manual', work_id: 'w9' }) });
    expect(overridePayload(b, { hidden: true })).toEqual({
      source_path: '/root/b',
      hidden: true,
      work_id: 'w9',
    });
  });

  it('unhides while preserving the current work id', () => {
    const b = book({ path: '/b', hidden: true });
    expect(overridePayload(b, { hidden: false })).toEqual({
      source_path: '/root/b',
      hidden: false,
      work_id: '',
    });
  });

  it('sets a manual match while preserving the current hidden flag', () => {
    const b = book({ path: '/b', hidden: true });
    expect(overridePayload(b, { workId: 'w1' })).toEqual({
      source_path: '/root/b',
      hidden: true,
      work_id: 'w1',
    });
  });

  it('clears a match with an empty work id', () => {
    const b = book({ path: '/b', coverage: cov({ matched_by: 'manual', work_id: 'w9' }) });
    expect(overridePayload(b, { workId: '' })).toEqual({
      source_path: '/root/b',
      hidden: false,
      work_id: '',
    });
  });
});

describe('summarizeTally', () => {
  it('joins the non-zero buckets', () => {
    expect(summarizeTally({ created: 2, conflicts: 1, failed: 0 })).toBe(
      '2 enqueued, 1 already enqueued.',
    );
    expect(summarizeTally({ created: 0, conflicts: 0, failed: 3 })).toBe('3 failed.');
    expect(summarizeTally({ created: 0, conflicts: 0, failed: 0 })).toBe('Nothing to enqueue.');
  });
});

// parsePos is a contract mirror of the Go scheduler.ParseSeriesPos - these cases are the
// agreed grammar both sides must produce identically.
describe('parsePos (mirrors Go scheduler.ParseSeriesPos)', () => {
  it('parses integers and decimals', () => {
    expect(parsePos('1')).toBe(1);
    expect(parsePos('12')).toBe(12);
    expect(parsePos('2.5')).toBe(2.5);
    expect(parsePos('0.5')).toBe(0.5);
    expect(parsePos('.5')).toBe(0.5); // Go ParseFloat(".5") = 0.5
    expect(parsePos('1.')).toBe(1); // Go ParseFloat("1.") = 1
  });

  it('takes the leading number of an omnibus range (stops at the first non-[0-9.])', () => {
    expect(parsePos('1-3.5')).toBe(1);
    expect(parsePos('3.5-4')).toBe(3.5);
    expect(parsePos('2 and 3')).toBe(2);
  });

  it('returns the sentinel for empty, garbage, or multi-dot (Go ParseFloat rejects)', () => {
    expect(parsePos('')).toBe(POS_SENTINEL);
    expect(parsePos(undefined)).toBe(POS_SENTINEL);
    expect(parsePos('bonus')).toBe(POS_SENTINEL);
    expect(parsePos('.')).toBe(POS_SENTINEL);
    expect(parsePos('1.2.3')).toBe(POS_SENTINEL); // diverged before: JS returned 1.2
    expect(parsePos('..5')).toBe(POS_SENTINEL);
  });

  it('sorts the sentinel last', () => {
    const positions = ['bonus', '2', '1.2.3', '1'].map(parsePos);
    expect(Math.min(...positions)).toBe(1);
    expect(parsePos('bonus')).toBeGreaterThan(parsePos('999'));
  });
});

describe('tallyResults', () => {
  const r = (partial: Partial<BookCreateResult>): BookCreateResult => ({
    source_path: partial.source_path ?? '/x',
    created: partial.created ?? false,
    ...partial,
  });

  it('buckets created / conflict / failed', () => {
    const results = [
      r({ created: true }),
      r({ created: true }),
      r({ conflict: true }),
      r({ error: 'boom' }), // neither created nor conflict => failed
    ];
    expect(tallyResults(results)).toEqual({ created: 2, conflicts: 1, failed: 1 });
  });

  it('is all-zero for an empty list', () => {
    expect(tallyResults([])).toEqual({ created: 0, conflicts: 0, failed: 0 });
  });
});

describe('searchCandidates', () => {
  const dune = book({
    path: '/d',
    title: 'Dune',
    authors: ['Frank Herbert'],
    series: 'Dune Chronicles',
    narrators: ['Scott Brick'],
    asin: 'B0011',
  });
  const hobbit = book({
    path: '/h',
    title: 'The Hobbit',
    authors: ['J.R.R. Tolkien'],
    narrators: ['Rob Inglis'],
    isbn: '9780261102217',
  });
  const books = [dune, hobbit];

  it('returns the same array reference for a blank / whitespace query', () => {
    expect(searchCandidates(books, '')).toBe(books);
    expect(searchCandidates(books, '   ')).toBe(books);
  });

  it('matches case-insensitively against the title', () => {
    expect(searchCandidates(books, 'dUnE')).toEqual([dune]);
  });

  it('matches on author, narrator, series, asin, and isbn', () => {
    expect(searchCandidates(books, 'herbert')).toEqual([dune]);
    expect(searchCandidates(books, 'brick')).toEqual([dune]);
    expect(searchCandidates(books, 'chronicles')).toEqual([dune]);
    expect(searchCandidates(books, 'b0011')).toEqual([dune]);
    expect(searchCandidates(books, '9780261102217')).toEqual([hobbit]);
  });

  it('requires ALL tokens to match somewhere (AND semantics)', () => {
    // Both tokens appear in Dune (title + narrator), so it matches...
    expect(searchCandidates(books, 'dune brick')).toEqual([dune]);
    // ...but a token that matches no book drops it.
    expect(searchCandidates(books, 'dune tolkien')).toEqual([]);
  });

  it('preserves input order and returns [] when nothing matches', () => {
    const a = book({ path: '/a', title: 'Alpha', series: 'Common' });
    const b = book({ path: '/b', title: 'Beta', series: 'Common' });
    expect(searchCandidates([b, a], 'common')).toEqual([b, a]);
    expect(searchCandidates(books, 'zzzznope')).toEqual([]);
  });

  it('tolerates books with missing optional fields', () => {
    const bare = book({ path: '/bare', title: 'Bare', authors: undefined, narrators: undefined });
    expect(searchCandidates([bare], 'bare')).toEqual([bare]);
    expect(searchCandidates([bare], 'nope')).toEqual([]);
  });
});

describe('sortBySeries', () => {
  it('groups by series then orders by parsed position, with title as tiebreak', () => {
    const s1 = book({ path: '/s1', title: 'First', series: 'Saga', series_position: '1' });
    const s25 = book({ path: '/s25', title: 'Interlude', series: 'Saga', series_position: '2.5' });
    const s2 = book({ path: '/s2', title: 'Second', series: 'Saga', series_position: '2' });
    // Input deliberately out of order.
    const out = sortBySeries([s25, s2, s1]);
    expect(out.map((b) => b.path)).toEqual(['/s1', '/s2', '/s25']);
  });

  it('does not mutate the input array', () => {
    const input = [
      book({ path: '/b', title: 'B', series: 'X', series_position: '2' }),
      book({ path: '/a', title: 'A', series: 'X', series_position: '1' }),
    ];
    const snapshot = input.map((b) => b.path);
    sortBySeries(input);
    expect(input.map((b) => b.path)).toEqual(snapshot);
  });

  it('places seriesless books after all series-grouped books, ordered by title', () => {
    const s = book({ path: '/s', title: 'In A Series', series: 'Saga', series_position: '1' });
    const loose2 = book({ path: '/l2', title: 'Zephyr' });
    const loose1 = book({ path: '/l1', title: 'Apple' });
    const out = sortBySeries([loose2, s, loose1]);
    expect(out.map((b) => b.path)).toEqual(['/s', '/l1', '/l2']);
  });

  it('groups series names case-insensitively', () => {
    const upper = book({ path: '/u', title: 'U', series: 'Saga', series_position: '2' });
    const lower = book({ path: '/l', title: 'L', series: 'saga', series_position: '1' });
    const out = sortBySeries([upper, lower]);
    // Both belong to one (case-insensitive) group, ordered by position.
    expect(out.map((b) => b.path)).toEqual(['/l', '/u']);
  });

  it('sorts a blank / unparseable position last within its series', () => {
    const noPos = book({ path: '/np', title: 'Bonus', series: 'Saga', series_position: '' });
    const p1 = book({ path: '/p1', title: 'One', series: 'Saga', series_position: '1' });
    const out = sortBySeries([noPos, p1]);
    expect(out.map((b) => b.path)).toEqual(['/p1', '/np']);
  });

  it('handles omnibus ranges by their leading number', () => {
    const omni = book({ path: '/o', title: 'Omnibus', series: 'Saga', series_position: '1-3' });
    const four = book({ path: '/4', title: 'Four', series: 'Saga', series_position: '4' });
    // "1-3" parses to 1, so it sorts before position 4.
    expect(sortBySeries([four, omni]).map((b) => b.path)).toEqual(['/o', '/4']);
  });

  it('orders distinct series alphabetically (case-insensitive)', () => {
    const zed = book({ path: '/z', title: 'Z', series: 'Zed', series_position: '1' });
    const alp = book({ path: '/a', title: 'A', series: 'alpha', series_position: '1' });
    expect(sortBySeries([zed, alp]).map((b) => b.path)).toEqual(['/a', '/z']);
  });

  it('is a no-op-shaped total order on an empty list', () => {
    expect(sortBySeries([])).toEqual([]);
  });
});

describe('seriesGapHint', () => {
  const b1 = book({ path: '/s1', series: 'Saga', series_position: '1' });
  const b2 = book({ path: '/s2', series: 'Saga', series_position: '2' });
  const b3 = book({ path: '/s3', series: 'Saga', series_position: '3' });

  it('flags a series when a selected book skips an earlier unselected one', () => {
    const hint = seriesGapHint([b1, b2, b3], new Set(['/s2']));
    expect(hint).toEqual(['Saga']);
  });

  it('does not flag when the earliest present book is selected', () => {
    expect(seriesGapHint([b1, b2], new Set(['/s1', '/s2']))).toEqual([]);
  });

  it('ignores seriesless books and empty selections', () => {
    const loose = book({ path: '/loose' });
    expect(seriesGapHint([loose, b2], new Set(['/s2']))).toEqual([]);
    expect(seriesGapHint([b1, b2], new Set())).toEqual([]);
  });

  it('reports multiple affected series, sorted', () => {
    const z1 = book({ path: '/z1', series: 'Zed', series_position: '1' });
    const z2 = book({ path: '/z2', series: 'Zed', series_position: '2' });
    const hint = seriesGapHint([b1, b2, z1, z2], new Set(['/s2', '/z2']));
    expect(hint).toEqual(['Saga', 'Zed']);
  });
});
