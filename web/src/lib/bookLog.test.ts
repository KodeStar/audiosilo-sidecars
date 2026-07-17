import { describe, it, expect } from 'vitest';
import type { LoggedEvent } from '@/api/types';
import { formatLogEvent, formatLogTime } from './bookLog';

function ev(partial: Partial<LoggedEvent>): LoggedEvent {
  return {
    id: partial.id ?? 1,
    ts: partial.ts ?? '2026-07-17T12:00:00Z',
    type: partial.type ?? 'book.state',
    payload: partial.payload,
  };
}

describe('formatLogEvent', () => {
  it('summarizes a plain state advance', () => {
    expect(formatLogEvent(ev({ type: 'book.state', payload: { state: 'asr', status: '' } }))).toBe(
      'state -> asr',
    );
  });

  it('appends a status and error for an exceptional state', () => {
    expect(
      formatLogEvent(
        ev({
          type: 'book.state',
          payload: { state: 'markers_normalizing', status: 'needs_attention', error: 'no markers' },
        }),
      ),
    ).toBe('state -> markers_normalizing (needs_attention: no markers)');
  });

  it('appends just the status when there is no error', () => {
    expect(
      formatLogEvent(ev({ type: 'book.state', payload: { state: 'asr', status: 'paused' } })),
    ).toBe('state -> asr (paused)');
  });

  it('summarizes stage progress', () => {
    expect(
      formatLogEvent(ev({ type: 'stage.progress', payload: { stage: 'asr', done: 3, total: 84 } })),
    ).toBe('asr 3/84');
  });

  it('falls back to ? for a missing state / stage field', () => {
    expect(formatLogEvent(ev({ type: 'book.state', payload: {} }))).toBe('state -> ?');
    expect(formatLogEvent(ev({ type: 'stage.progress', payload: {} }))).toBe('? 0/0');
  });

  it('falls back to the raw type for an unknown event', () => {
    expect(formatLogEvent(ev({ type: 'queue.stats', payload: { queued: 2 } }))).toBe('queue.stats');
    expect(formatLogEvent(ev({ type: 'mystery', payload: undefined }))).toBe('mystery');
  });
});

describe('formatLogTime', () => {
  it('returns a non-empty time string for a valid timestamp', () => {
    expect(formatLogTime('2026-07-17T12:00:00Z')).not.toBe('');
  });

  it('returns empty for an unparseable timestamp', () => {
    expect(formatLogTime('not-a-date')).toBe('');
  });
});
