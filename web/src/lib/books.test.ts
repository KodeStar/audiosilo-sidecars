import { describe, it, expect } from 'vitest';
import type { BookView } from '@/api/types';
import {
  applyBookState,
  applyStageProgress,
  availableActions,
  formatBytes,
  isDone,
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
  it('keeps done books last and newest active first', () => {
    const a = bk({ id: 1, state: 'asr', created_at: '2026-01-01T00:00:00Z' });
    const b = bk({ id: 2, state: 'asr', created_at: '2026-01-02T00:00:00Z' });
    const d = bk({ id: 3, state: 'done', created_at: '2026-01-03T00:00:00Z' });
    const out = sortBooks([a, d, b]);
    expect(out.map((x) => x.id)).toEqual([2, 1, 3]);
  });
});
