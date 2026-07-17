import { describe, it, expect } from 'vitest';
import { parseTimestamp } from './time';

describe('parseTimestamp', () => {
  it('parses a valid RFC3339 timestamp to epoch ms', () => {
    expect(parseTimestamp('2026-07-17T00:00:00Z')).toBe(Date.parse('2026-07-17T00:00:00Z'));
  });

  it('returns null for an empty string', () => {
    expect(parseTimestamp('')).toBeNull();
  });

  it('returns null for an unparseable value', () => {
    expect(parseTimestamp('not-a-date')).toBeNull();
  });
});
