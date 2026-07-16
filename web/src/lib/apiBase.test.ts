import { describe, it, expect } from 'vitest';
import { resolveApiBase } from './apiBase';

describe('resolveApiBase', () => {
  it('returns the api_base when present', () => {
    expect(resolveApiBase({ api_base: 'https://host:8090' })).toBe('https://host:8090');
  });

  it('trims a trailing slash from api_base', () => {
    expect(resolveApiBase({ api_base: 'https://host:8090/' })).toBe('https://host:8090');
  });

  it('falls back to same-origin when api_base is missing', () => {
    expect(resolveApiBase({})).toBe('');
  });

  it('falls back to same-origin when api_base is empty', () => {
    expect(resolveApiBase({ api_base: '' })).toBe('');
    expect(resolveApiBase({ api_base: '   ' })).toBe('');
  });

  it('falls back to same-origin for a non-object config', () => {
    expect(resolveApiBase(null)).toBe('');
    expect(resolveApiBase('nope')).toBe('');
    expect(resolveApiBase(42)).toBe('');
    expect(resolveApiBase(undefined)).toBe('');
  });
});
