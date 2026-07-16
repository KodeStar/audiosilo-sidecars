import { describe, it, expect, beforeEach } from 'vitest';
import { addRecentRoot, loadRecentRoots } from './recentRoots';

describe('recentRoots', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('starts empty', () => {
    expect(loadRecentRoots()).toEqual([]);
  });

  it('adds most-recent-first and de-duplicates', () => {
    addRecentRoot('/a');
    addRecentRoot('/b');
    expect(addRecentRoot('/a')).toEqual(['/a', '/b']);
    expect(loadRecentRoots()).toEqual(['/a', '/b']);
  });

  it('ignores blank paths', () => {
    addRecentRoot('/a');
    expect(addRecentRoot('   ')).toEqual(['/a']);
  });

  it('caps the history length', () => {
    for (let i = 0; i < 12; i++) addRecentRoot(`/root-${i}`);
    const roots = loadRecentRoots();
    expect(roots).toHaveLength(8);
    expect(roots[0]).toBe('/root-11');
  });

  it('tolerates corrupt storage', () => {
    localStorage.setItem('audiosilo.sidecars.recentRoots', 'not json');
    expect(loadRecentRoots()).toEqual([]);
  });
});
