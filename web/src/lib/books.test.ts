import { describe, it, expect } from 'vitest';
import type { BookView } from '@/api/types';
import { applyBookState, applyStageProgress, availableActions, isDone, sortBooks } from './books';

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
    created_at: partial.created_at ?? '2026-01-01T00:00:00Z',
    updated_at: partial.updated_at ?? '2026-01-01T00:00:00Z',
    ...partial,
  };
}

describe('applyBookState', () => {
  it('patches the matching book state + lane + status', () => {
    const books = [bk({ id: 1, state: 'queued' }), bk({ id: 2, state: 'asr', lane: 'asr' })];
    const out = applyBookState(books, {
      book_id: 2,
      state: 'sanitizing',
      lane: 'mechanical',
      status: 'paused',
    });
    expect(out[1].state).toBe('sanitizing');
    expect(out[1].lane).toBe('mechanical'); // the served lane rides along the patch
    expect(out[1].status).toBe('paused');
    expect(out[0]).toBe(books[0]); // untouched reference
  });

  it('returns the same array reference when no book matches', () => {
    const books = [bk({ id: 1 })];
    const out = applyBookState(books, { book_id: 99, state: 'done', lane: '', status: '' });
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
  it('derives controls from status/state', () => {
    expect(availableActions(bk({ state: 'done' }))).toEqual(['delete']);
    expect(availableActions(bk({ status: 'paused' }))).toEqual(['resume', 'cancel']);
    expect(availableActions(bk({ status: 'failed' }))).toEqual(['retry', 'delete']);
    expect(availableActions(bk({ status: 'needs_attention' }))).toEqual(['retry', 'cancel']);
    expect(availableActions(bk({ state: 'asr', status: '' }))).toEqual(['pause', 'cancel']);
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
