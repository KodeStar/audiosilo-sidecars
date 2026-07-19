import { describe, it, expect } from 'vitest';
import type { BookView } from '@/api/types';
import {
  applyBookState,
  applyEtaUpdate,
  applyQueueSnapshot,
  applyQueueStats,
  applyStageProgress,
  availableActions,
  bumpBookEventCount,
  formatBytes,
  groupRunningBooks,
  isDone,
  pruneBookEventCounts,
  sortBooks,
} from './books';

function bk(partial: Partial<BookView>): BookView {
  return {
    id: partial.id ?? 1,
    source_path: partial.source_path ?? '/x',
    title: partial.title ?? 'T',
    authors: partial.authors ?? [],
    state: partial.state ?? 'queued',
    lane: partial.lane ?? '',
    status: partial.status ?? '',
    progress: partial.progress ?? [],
    scratch_bytes: partial.scratch_bytes ?? 0,
    total_cost_usd: partial.total_cost_usd ?? 0,
    created_at: partial.created_at ?? '2026-01-01T00:00:00Z',
    updated_at: partial.updated_at ?? '2026-01-01T00:00:00Z',
    ...partial,
  };
}

describe('applyBookState', () => {
  it('patches the matching book state + lane + status + error', () => {
    const books = [bk({ id: 1, state: 'queued' }), bk({ id: 2, state: 'asr', lane: 'asr' })];
    const out = applyBookState(books, {
      book_id: 2,
      state: 'sanitizing',
      lane: 'mechanical',
      status: 'failed',
      error: 'boom',
    });
    expect(out[1].state).toBe('sanitizing');
    expect(out[1].lane).toBe('mechanical'); // the served lane rides along the patch
    expect(out[1].status).toBe('failed');
    expect(out[1].error).toBe('boom'); // the error rides along too (F8)
    expect(out[0]).toBe(books[0]); // untouched reference
  });

  it('clears the error when an advance publishes an empty error', () => {
    const books = [bk({ id: 1, state: 'asr', status: 'failed', error: 'old failure' })];
    const out = applyBookState(books, {
      book_id: 1,
      state: 'sanitizing',
      lane: 'mechanical',
      status: '',
      error: '',
    });
    expect(out[0].status).toBe('');
    expect(out[0].error).toBe('');
  });

  it('preserves queue placement until a queue snapshot actually changes it', () => {
    const books = [
      bk({
        id: 1,
        state: 'fact_pass',
        queue_group: 'processing',
        queue_position: 1,
        queue_active: true,
      }),
    ];
    const out = applyBookState(books, {
      book_id: 1,
      state: 'synthesizing',
      lane: 'agent',
      status: '',
      error: '',
    });
    expect(out[0]).toMatchObject({
      queue_group: 'processing',
      queue_position: 1,
      queue_active: true,
    });
  });

  it('carries park_code from the event (set on park, cleared on advance)', () => {
    const books = [bk({ id: 1, state: 'markers_normalizing', status: '' })];
    const parked = applyBookState(books, {
      book_id: 1,
      state: 'markers_normalizing',
      lane: 'agent',
      status: 'needs_attention',
      error: 'no confident markers',
      park_code: 'markers_not_confident',
    });
    expect(parked[0].park_code).toBe('markers_not_confident');
    // A later advance with no park_code clears it.
    const advanced = applyBookState(parked, {
      book_id: 1,
      state: 'splitting',
      lane: 'mechanical',
      status: '',
      error: '',
    });
    expect(advanced[0].park_code).toBeUndefined();
  });

  it('returns the same array reference when no book matches', () => {
    const books = [bk({ id: 1 })];
    const out = applyBookState(books, {
      book_id: 99,
      state: 'done',
      lane: '',
      status: '',
      error: '',
    });
    expect(out).toBe(books);
  });

  it('mirrors retry_at from the event: sets it on a timed park, clears a stale one on a park without it', () => {
    const books = [bk({ id: 1, state: 'fact_pass', status: '' })];
    // A transient-agent park carrying a scheduled auto-readmit instant.
    const parked = applyBookState(books, {
      book_id: 1,
      state: 'fact_pass',
      lane: 'agent',
      status: 'needs_attention',
      error: 'rate limited',
      park_code: 'agent_rate_limited',
      retry_at: '2026-07-18T12:34:56Z',
    });
    expect(parked[0].retry_at).toBe('2026-07-18T12:34:56Z');
    // A later human-only park (no retry_at on the frame) must drop the stale instant to ''.
    const reparked = applyBookState(parked, {
      book_id: 1,
      state: 'fact_pass',
      lane: 'agent',
      status: 'needs_attention',
      error: 'backend unavailable',
      park_code: 'agent_unavailable',
    });
    expect(reparked[0].retry_at).toBe('');
  });
});

describe('applyEtaUpdate', () => {
  it('patches listed books and clears the ETA on unlisted books', () => {
    const books = [bk({ id: 1, eta_seconds: 100 }), bk({ id: 2, eta_seconds: 200 }), bk({ id: 3 })];
    const out = applyEtaUpdate(books, {
      queue_seconds: 5400,
      books: [
        { book_id: 1, eta_seconds: 150 },
        { book_id: 3, eta_seconds: 300 },
      ],
    });
    expect(out[0].eta_seconds).toBe(150); // updated
    expect(out[1].eta_seconds).toBeUndefined(); // unlisted -> cleared (parked/terminal)
    expect(out[2].eta_seconds).toBe(300); // newly gained an ETA
  });

  it('ignores unknown book ids and returns the same reference when unchanged', () => {
    const books = [bk({ id: 1, eta_seconds: 100 })];
    const out = applyEtaUpdate(books, {
      queue_seconds: null,
      books: [
        { book_id: 1, eta_seconds: 100 }, // same value
        { book_id: 99, eta_seconds: 500 }, // unknown, ignored
      ],
    });
    expect(out).toBe(books);
  });
});

describe('applyQueueSnapshot', () => {
  it('replaces live placements and clears books omitted from the scheduler snapshot', () => {
    const books = [
      bk({
        id: 1,
        queue_group: 'asr',
        queue_bucket: 'transcribing',
        queue_position: 1,
        queue_active: true,
      }),
      bk({
        id: 2,
        queue_group: 'asr',
        queue_bucket: 'transcription',
        queue_position: 1,
        queue_active: false,
      }),
    ];
    const out = applyQueueSnapshot(books, {
      asr_active: 0,
      agent_active: 1,
      mechanical_active: 0,
      queued: 1,
      queue_books: [
        { book_id: 2, group: 'processing', bucket: 'agent_active', position: 1, active: true },
      ],
    });
    expect(out[0]).toMatchObject({
      queue_group: undefined,
      queue_bucket: undefined,
      queue_position: undefined,
      queue_active: undefined,
    });
    expect(out[1]).toMatchObject({
      queue_group: 'processing',
      queue_bucket: 'agent_active',
      queue_position: 1,
      queue_active: true,
    });
  });

  it('leaves placements untouched when an older daemon omits queue_books', () => {
    const books = [bk({ id: 1, queue_group: 'asr', queue_position: 1 })];
    expect(
      applyQueueSnapshot(books, {
        asr_active: 1,
        agent_active: 0,
        mechanical_active: 0,
        queued: 1,
      }),
    ).toBe(books);
  });
});

describe('applyQueueStats', () => {
  it('preserves array and row identity when a full queue frame changes nothing', () => {
    const blocker = { book_id: 9, title: 'Earlier', series_pos: '1' };
    const books = [
      bk({
        id: 1,
        queue_group: 'processing',
        queue_bucket: 'agent_active',
        queue_position: 1,
        queue_active: true,
        active_agent_invocations: 2,
        series_blocked_by: blocker,
      }),
    ];
    const out = applyQueueStats(books, {
      asr_active: 0,
      agent_active: 1,
      mechanical_active: 0,
      queued: 1,
      agent_invocations_by_book: { '1': 2 },
      series_blocked_by: { '1': { ...blocker } },
      queue_books: [
        { book_id: 1, group: 'processing', bucket: 'agent_active', position: 1, active: true },
      ],
    });
    expect(out).toBe(books);
    expect(out[0]).toBe(books[0]);
  });

  it('clones only the row whose occupancy changed', () => {
    const books = [
      bk({ id: 1, active_agent_invocations: 0 }),
      bk({ id: 2, active_agent_invocations: 0 }),
    ];
    const out = applyQueueStats(books, {
      asr_active: 0,
      agent_active: 1,
      mechanical_active: 0,
      queued: 2,
      agent_invocations_by_book: { '1': 1, '2': 0 },
    });
    expect(out[0]).not.toBe(books[0]);
    expect(out[0].active_agent_invocations).toBe(1);
    expect(out[1]).toBe(books[1]);
  });
});

describe('applyStageProgress', () => {
  it('inserts then updates a stage counter', () => {
    let books = [bk({ id: 1, state: 'asr' })];
    books = applyStageProgress(books, { book_id: 1, stage: 'asr', done: 1, total: 10 });
    expect(books[0].progress).toEqual([{ stage: 'asr', done: 1, total: 10 }]);
    books = applyStageProgress(books, { book_id: 1, stage: 'asr', done: 5, total: 10 });
    expect(books[0].progress).toEqual([{ stage: 'asr', done: 5, total: 10 }]);
  });

  it('ignores events for unknown books', () => {
    const books = [bk({ id: 1 })];
    expect(applyStageProgress(books, { book_id: 2, stage: 'asr', done: 1, total: 2 })).toBe(books);
  });
});

describe('bumpBookEventCount', () => {
  it('starts a book at 1 and increments on each frame', () => {
    let counts: Record<number, number> = {};
    counts = bumpBookEventCount(counts, 7);
    expect(counts[7]).toBe(1);
    counts = bumpBookEventCount(counts, 7);
    expect(counts[7]).toBe(2);
  });

  it('tracks books independently and returns a new record each time', () => {
    const a: Record<number, number> = {};
    const b = bumpBookEventCount(a, 1);
    const c = bumpBookEventCount(b, 2);
    expect(b).not.toBe(a); // new reference so a memoized row re-renders
    expect(a).toEqual({}); // original untouched
    expect(c).toEqual({ 1: 1, 2: 1 });
  });
});

describe('pruneBookEventCounts', () => {
  it('drops counter keys not in the live-id set', () => {
    const counts = { 1: 3, 2: 1, 3: 5 };
    expect(pruneBookEventCounts(counts, [1, 3])).toEqual({ 1: 3, 3: 5 });
  });

  it('returns the same reference when nothing needs dropping', () => {
    const counts = { 1: 3, 2: 1 };
    expect(pruneBookEventCounts(counts, [1, 2, 4])).toBe(counts);
    expect(pruneBookEventCounts({}, [1])).toEqual({});
  });

  it('accepts a Set of live ids', () => {
    const counts = { 1: 3, 2: 1 };
    expect(pruneBookEventCounts(counts, new Set([2]))).toEqual({ 2: 1 });
  });
});

describe('isDone', () => {
  it('is true only for the terminal state', () => {
    expect(isDone(bk({ state: 'done' }))).toBe(true);
    expect(isDone(bk({ state: 'ready' }))).toBe(false);
  });
});

describe('availableActions', () => {
  it('derives controls from status/state (no scratch)', () => {
    expect(availableActions(bk({ state: 'done' }))).toEqual(['delete']);
    expect(availableActions(bk({ status: 'paused' }))).toEqual(['resume', 'cancel']);
    expect(availableActions(bk({ status: 'failed' }))).toEqual(['retry', 'delete']);
    expect(availableActions(bk({ status: 'needs_attention' }))).toEqual(['retry', 'cancel']);
    expect(availableActions(bk({ state: 'asr', status: '' }))).toEqual(['pause', 'cancel']);
  });

  it('offers purge only when the book is purgeable AND holds scratch', () => {
    // Done / paused / failed with scratch: purge sits before the destructive action.
    expect(availableActions(bk({ state: 'done', scratch_bytes: 1024 }))).toEqual([
      'purge',
      'delete',
    ]);
    expect(availableActions(bk({ status: 'paused', scratch_bytes: 1024 }))).toEqual([
      'resume',
      'purge',
      'cancel',
    ]);
    expect(availableActions(bk({ status: 'failed', scratch_bytes: 1024 }))).toEqual([
      'retry',
      'purge',
      'delete',
    ]);
    // needs_attention keeps its chapters (may retranscribe/fix) - no purge.
    expect(availableActions(bk({ status: 'needs_attention', scratch_bytes: 1024 }))).toEqual([
      'retry',
      'cancel',
    ]);
    // A running book is never purgeable, even with scratch.
    expect(availableActions(bk({ state: 'asr', status: '', scratch_bytes: 1024 }))).toEqual([
      'pause',
      'cancel',
    ]);
  });
});

describe('formatBytes', () => {
  it('renders binary units with a short label', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(-5)).toBe('0 B');
    expect(formatBytes(512)).toBe('512 B');
    expect(formatBytes(1024)).toBe('1 KB');
    expect(formatBytes(1536)).toBe('1.5 KB');
    expect(formatBytes(1024 * 1024)).toBe('1 MB');
    expect(formatBytes(3.2 * 1024 * 1024 * 1024)).toBe('3.2 GB');
  });
});

describe('sortBooks', () => {
  it('uses served Processing and ASR positions, then exceptional and completed sections', () => {
    const processingNow = bk({
      id: 1,
      state: 'fact_pass',
      lane: 'agent',
      queue_group: 'processing',
      queue_bucket: 'agent_active',
      queue_position: 1,
      queue_active: true,
    });
    const processingNext = bk({
      id: 2,
      state: 'spelling_research',
      lane: 'agent',
      queue_group: 'processing',
      queue_bucket: 'agent',
      queue_position: 1,
    });
    const asrNow = bk({
      id: 3,
      state: 'asr',
      lane: 'asr',
      queue_group: 'asr',
      queue_bucket: 'transcribing',
      queue_position: 1,
      queue_active: true,
    });
    const asrNext = bk({
      id: 4,
      state: 'asr',
      lane: 'asr',
      queue_group: 'asr',
      queue_bucket: 'transcription',
      queue_position: 1,
    });
    const paused = bk({ id: 5, state: 'asr', status: 'paused' });
    const parked = bk({ id: 6, state: 'auditing', status: 'needs_attention' });
    const failed = bk({ id: 7, state: 'asr', status: 'failed' });
    const done = bk({ id: 8, state: 'done' });

    const out = sortBooks([
      done,
      failed,
      parked,
      paused,
      asrNext,
      processingNext,
      asrNow,
      processingNow,
    ]);
    expect(out.map((x) => x.id)).toEqual([1, 2, 3, 4, 5, 6, 7, 8]);
  });

  it('keeps a deterministic stage-then-id fallback when queue positions are absent', () => {
    const a = bk({
      id: 1,
      state: 'asr',
      lane: 'asr',
      status: '',
    });
    const b = bk({
      id: 2,
      state: 'splitting',
      lane: 'mechanical',
      status: '',
    });
    expect(sortBooks([b, a]).map((x) => x.id)).toEqual([1, 2]);
  });

  it('keeps independent scheduler buckets in their served display order', () => {
    const books = [
      bk({ id: 4, queue_group: 'asr', queue_bucket: 'retranscription', queue_position: 1 }),
      bk({ id: 3, queue_group: 'asr', queue_bucket: 'transcription', queue_position: 1 }),
      bk({
        id: 2,
        queue_group: 'asr',
        queue_bucket: 'retranscribing',
        queue_position: 1,
        queue_active: true,
      }),
      bk({
        id: 1,
        queue_group: 'asr',
        queue_bucket: 'transcribing',
        queue_position: 1,
        queue_active: true,
      }),
    ];
    expect(sortBooks(books).map((book) => book.id)).toEqual([1, 2, 3, 4]);
  });

  it('keeps a cooperatively paused in-flight book in its active queue', () => {
    const sections = groupRunningBooks([
      bk({
        id: 1,
        status: 'paused',
        queue_group: 'asr',
        queue_bucket: 'transcribing',
        queue_position: 1,
        queue_active: true,
      }),
      bk({ id: 2, status: 'paused' }),
    ]);
    expect(sections.map((section) => [section.key, section.books.map((book) => book.id)])).toEqual([
      ['asr', [1]],
      ['paused', [2]],
    ]);
  });

  it('groups the ordered books into labelled, non-empty sections', () => {
    const sections = groupRunningBooks([
      bk({ id: 4, state: 'done' }),
      bk({ id: 3, state: 'asr', queue_group: 'asr', queue_position: 1 }),
      bk({
        id: 2,
        state: 'fact_pass',
        queue_group: 'processing',
        queue_position: 2,
      }),
      bk({
        id: 1,
        state: 'synthesizing',
        queue_group: 'processing',
        queue_position: 1,
      }),
    ]);
    expect(
      sections.map((section) => [section.label, section.books.map((book) => book.id)]),
    ).toEqual([
      ['Processing', [1, 2]],
      ['ASR', [3]],
      ['Completed', [4]],
    ]);
  });

  it('uses the off-mainline anchor when falling back without served queue fields', () => {
    const sections = groupRunningBooks([
      bk({ id: 1, state: 'markers_normalizing', lane: 'agent' }),
      bk({ id: 2, state: 'qa_adjudicating', lane: 'agent' }),
    ]);
    expect(sections.map((section) => [section.key, section.books.map((book) => book.id)])).toEqual([
      ['processing', [2]],
      ['asr', [1]],
    ]);
  });
});
