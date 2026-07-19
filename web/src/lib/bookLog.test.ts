import { describe, it, expect } from 'vitest';
import type { BookEventsResponse, LoggedEvent } from '@/api/types';
import { fetchAllEvents, formatLogEvent, formatLogTime, logToText } from './bookLog';

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

  it('renders a stage.note prefixed with its stage', () => {
    expect(
      formatLogEvent(
        ev({
          type: 'stage.note',
          payload: { stage: 'retranscribing', msg: 're-transcribing 2 chapters: 2, 3' },
        }),
      ),
    ).toBe('retranscribing: re-transcribing 2 chapters: 2, 3');
  });

  it('does not double-prefix a stage.note that already leads with its stage', () => {
    expect(
      formatLogEvent(
        ev({
          type: 'stage.note',
          payload: { stage: 'asr', msg: 'asr: still running (2m elapsed)' },
        }),
      ),
    ).toBe('asr: still running (2m elapsed)');
  });

  it('falls back to the raw type for a stage.note with no message', () => {
    expect(formatLogEvent(ev({ type: 'stage.note', payload: { stage: 'asr' } }))).toBe(
      'stage.note',
    );
  });

  it('renders a supervisor decision with its action and concrete evidence', () => {
    expect(
      formatLogEvent(
        ev({
          type: 'supervisor.decision',
          payload: {
            diagnosis: 'required artifact is missing',
            selected_action: 'park_escalate',
            evidence: ['_done/auditing.json', 'no such file'],
          },
        }),
      ),
    ).toBe(
      'supervisor: required artifact is missing -> park_escalate; evidence: _done/auditing.json; no such file',
    );
  });
});

describe('fetchAllEvents', () => {
  // fakeGetEvents serves a fixed newest-first pool of events via the before_id cursor,
  // paging like the real endpoint so the assembly logic can be exercised offline.
  function fakeGetEvents(pool: LoggedEvent[]) {
    // pool is newest-first (descending id). Track the calls for assertions.
    const calls: Array<{ limit?: number; beforeId?: number }> = [];
    const get = (_id: number, limit?: number, beforeId?: number): Promise<BookEventsResponse> => {
      calls.push({ limit, beforeId });
      let rows = pool;
      if (beforeId !== undefined) rows = rows.filter((e) => e.id < beforeId);
      return Promise.resolve({ events: rows.slice(0, limit ?? rows.length) });
    };
    return { get, calls };
  }

  it('pages back through the whole history and returns it oldest-first', async () => {
    // 1200 events, newest-first ids 1200..1 -> three pages of 500/500/200.
    const pool: LoggedEvent[] = [];
    for (let id = 1200; id >= 1; id--) {
      pool.push({ id, ts: '2026-07-17T12:00:00Z', type: 'stage.progress', payload: { n: id } });
    }
    const { get, calls } = fakeGetEvents(pool);
    const all = await fetchAllEvents(get, 7);

    expect(all).toHaveLength(1200);
    // Chronological: oldest (id 1) first, newest (id 1200) last.
    expect(all[0].id).toBe(1);
    expect(all[all.length - 1].id).toBe(1200);
    // Three pages: newest (no cursor), then before the oldest of each prior page.
    expect(calls).toEqual([
      { limit: 500, beforeId: undefined },
      { limit: 500, beforeId: 701 },
      { limit: 500, beforeId: 201 },
    ]);
  });

  it('stops after a single short page', async () => {
    const pool: LoggedEvent[] = [
      { id: 3, ts: '2026-07-17T12:00:03Z', type: 'book.state', payload: {} },
      { id: 2, ts: '2026-07-17T12:00:02Z', type: 'book.state', payload: {} },
      { id: 1, ts: '2026-07-17T12:00:01Z', type: 'book.state', payload: {} },
    ];
    const { get, calls } = fakeGetEvents(pool);
    const all = await fetchAllEvents(get, 7);
    expect(all.map((e) => e.id)).toEqual([1, 2, 3]);
    expect(calls).toHaveLength(1);
  });
});

describe('logToText', () => {
  it('renders one line per event in the given order, trailing newline', () => {
    const text = logToText([
      ev({ id: 1, type: 'book.state', payload: { state: 'asr' } }),
      ev({ id: 2, type: 'stage.progress', payload: { stage: 'asr', done: 3, total: 84 } }),
    ]);
    const lines = text.split('\n');
    expect(text.endsWith('\n')).toBe(true);
    expect(lines[0]).toContain('state -> asr');
    expect(lines[1]).toContain('asr 3/84');
  });

  it('returns empty string for no events', () => {
    expect(logToText([])).toBe('');
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
