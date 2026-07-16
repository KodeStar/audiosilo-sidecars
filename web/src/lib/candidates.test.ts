import { describe, it, expect } from 'vitest';
import type { Coverage, ScannedBook } from '@/api/types';
import {
  coverageState,
  filterCandidates,
  isCovered,
  parsePos,
  seriesGapHint,
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
  it('maps scan fields to the POST /books candidate shape, defaulting missing ones', () => {
    const c = toCandidate(
      book({
        path: '/lib/b1',
        title: 'Book One',
        authors: ['Jane Roe'],
        series: 'Saga',
        series_position: '2',
        asin: 'B001',
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

describe('parsePos', () => {
  it('extracts the leading number, Infinity for empty/unparseable', () => {
    expect(parsePos('1')).toBe(1);
    expect(parsePos('2.5')).toBe(2.5);
    expect(parsePos('1-3.5')).toBe(1);
    expect(parsePos('')).toBe(Infinity);
    expect(parsePos(undefined)).toBe(Infinity);
    expect(parsePos('bonus')).toBe(Infinity);
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
