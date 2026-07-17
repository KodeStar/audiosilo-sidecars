import { describe, expect, it } from 'vitest';
import type { ScanProgress } from '../api/types';
import { runningScanDetail } from './scanStatus';

function prog(partial: Partial<ScanProgress>): ScanProgress {
  return {
    phase: 'scanning',
    walk_dirs: 0,
    walk_groups: 0,
    groups_done: 0,
    groups_total: 0,
    books_found: 0,
    coverage_done: 0,
    coverage_total: 0,
    ...partial,
  };
}

describe('runningScanDetail', () => {
  it('shows bare "Scanning folders" before the walk has counted a directory', () => {
    expect(runningScanDetail(prog({}))).toBe('Scanning folders');
  });

  it('shows live walk counts while the tree is still enumerating (groups_total 0)', () => {
    expect(runningScanDetail(prog({ walk_dirs: 42, walk_groups: 5 }))).toBe(
      'Scanning folders - 42 folders, 5 books found',
    );
  });

  it('pluralizes walk counts correctly (singular)', () => {
    expect(runningScanDetail(prog({ walk_dirs: 1, walk_groups: 1 }))).toBe(
      'Scanning folders - 1 folder, 1 book found',
    );
  });

  it('switches to groups_done/groups_total once the tree is enumerated', () => {
    expect(runningScanDetail(prog({ walk_dirs: 42, groups_done: 3, groups_total: 12 }))).toBe(
      'Scanning folders 3/12',
    );
  });

  it('appends the streamed book count during the group pass', () => {
    expect(runningScanDetail(prog({ groups_done: 3, groups_total: 12, books_found: 2 }))).toBe(
      'Scanning folders 3/12 - 2 books found',
    );
  });

  it('shows coverage progress in the coverage phase', () => {
    expect(
      runningScanDetail(prog({ phase: 'coverage', coverage_done: 4, coverage_total: 10 })),
    ).toBe('Checking coverage 4/10');
  });

  it('shows bare coverage label before totals are known', () => {
    expect(runningScanDetail(prog({ phase: 'coverage' }))).toBe('Checking coverage');
  });
});
