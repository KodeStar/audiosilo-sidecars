import { describe, it, expect } from 'vitest';
import type { BookCreateResult, Coverage, ScannedBook } from '@/api/types';
import {
  coverageState,
  filterCandidates,
  isCovered,
  parsePos,
  POS_SENTINEL,
  seriesGapHint,
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
      source_path: '/lib/b1',
      title: 'Book One',
      authors: ['Jane Roe'],
      series: 'Saga',
      series_pos: '2',
      asin: 'B001',
      isbn: '',
      coverage,
      sources,
    });
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
});

// parsePos is a contract mirror of the Go parseSeriesPos - these cases are the
// agreed grammar both sides must produce identically.
describe('parsePos (mirrors Go parseSeriesPos)', () => {
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
