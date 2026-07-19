import { beforeEach, describe, expect, it } from 'vitest';
import type { SystemTab } from '@/api/types';
import { readTabFromURL, resolveTab, writeTabToURL } from './tabNavigation';

const tabs: SystemTab[] = [
  { id: 'library', label: 'Library', status: 'ready' },
  { id: 'running', label: 'Running', status: 'ready' },
  { id: 'future', label: 'Future', status: 'planned' },
];

beforeEach(() => {
  window.history.replaceState({}, '', '/');
});

describe('tab URL navigation', () => {
  it('honours a ready deep-linked tab', () => {
    window.history.replaceState({}, '', '/?tab=running');
    expect(readTabFromURL()).toBe('running');
    expect(resolveTab(tabs, readTabFromURL())).toBe('running');
  });

  it('falls back to the first ready tab for unknown or planned links', () => {
    expect(resolveTab(tabs, 'missing')).toBe('library');
    expect(resolveTab(tabs, 'future')).toBe('library');
  });

  it('updates tab without dropping other query parameters', () => {
    window.history.replaceState({}, '', '/?source=desktop');
    writeTabToURL('done', 'push');
    expect(window.location.search).toBe('?source=desktop&tab=done');
  });

  it('has a safe fallback when the daemon exposes no tabs', () => {
    expect(resolveTab([], null)).toBe('settings');
  });
});
