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
  seriesGapHint,
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
