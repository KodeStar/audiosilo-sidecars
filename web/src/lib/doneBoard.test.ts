import { describe, it, expect } from 'vitest';
import { filterDoneBooks, runDurationSeconds, stageRunRows } from './doneBoard';
import type { BookView, StageRun } from '@/api/types';

function book(partial: Partial<BookView>): BookView {
  return {
    id: 1,
    source_path: '/books/1',
    title: 'A Book',
    authors: [],
    state: 'done',
    lane: '',
    status: '',
    progress: [],
    scratch_bytes: 0,
    total_cost_usd: 0,
    created_at: '2026-07-17T00:00:00.000000000Z',
    updated_at: '2026-07-17T00:00:00.000000000Z',
    ...partial,
  };
}

function run(partial: Partial<StageRun>): StageRun {
  return {
    id: 1,
    book_id: 1,
    stage: 'fact_pass',
    attempt: 1,
    started_at: '2026-07-17T00:00:00.000000000Z',
    finished_at: '2026-07-17T00:01:00.000000000Z',
    ok: true,
    model: '',
    input_tokens: 0,
    output_tokens: 0,
    cost_usd: 0,
    ...partial,
  };
}

describe('filterDoneBooks', () => {
  it('keeps only done books', () => {
    const books = [
      book({ id: 1, state: 'done' }),
      book({ id: 2, state: 'asr' }),
      book({ id: 3, state: 'auditing' }),
      book({ id: 4, state: 'done' }),
    ];
    // Both done books share the default timestamp, so the id tiebreak (higher
    // first) orders them 4 then 1; the non-done books are dropped.
    expect(filterDoneBooks(books).map((b) => b.id)).toEqual([4, 1]);
  });

  it('orders newest-finished first, id-breaking ties', () => {
    const books = [
      book({ id: 1, updated_at: '2026-07-17T00:00:00.000000000Z' }),
      book({ id: 2, updated_at: '2026-07-17T05:00:00.000000000Z' }),
      book({ id: 3, updated_at: '2026-07-17T05:00:00.000000000Z' }),
    ];
    expect(filterDoneBooks(books).map((b) => b.id)).toEqual([3, 2, 1]);
  });

  it('does not mutate the input', () => {
    const books = [book({ id: 2, state: 'asr' }), book({ id: 1, state: 'done' })];
    const snapshot = books.map((b) => b.id);
    filterDoneBooks(books);
    expect(books.map((b) => b.id)).toEqual(snapshot);
  });
});

describe('runDurationSeconds', () => {
  it('computes the wallclock span in seconds', () => {
    expect(runDurationSeconds(run({}))).toBe(60);
    expect(
      runDurationSeconds(
        run({
          started_at: '2026-07-17T00:00:00.000000000Z',
          finished_at: '2026-07-17T00:04:12.000000000Z',
        }),
      ),
    ).toBe(252);
  });

  it('is null for a run still in flight (empty finished_at)', () => {
    expect(runDurationSeconds(run({ finished_at: '', ok: null }))).toBeNull();
  });

  it('is null for a missing / unparseable timestamp', () => {
    expect(runDurationSeconds(run({ started_at: '' }))).toBeNull();
    expect(runDurationSeconds(run({ finished_at: 'not-a-date' }))).toBeNull();
  });

  it('is null when the end precedes the start (clock skew)', () => {
    expect(
      runDurationSeconds(
        run({
          started_at: '2026-07-17T00:05:00.000000000Z',
          finished_at: '2026-07-17T00:00:00.000000000Z',
        }),
      ),
    ).toBeNull();
  });
});

describe('stageRunRows', () => {
  it('builds a labelled row per run, preserving order', () => {
    const rows = stageRunRows([
      run({ id: 1, stage: 'splitting', finished_at: '2026-07-17T00:00:04.000000000Z' }),
      run({
        id: 2,
        stage: 'fact_pass',
        model: 'claude-opus',
        input_tokens: 1200,
        output_tokens: 800,
        cost_usd: 0.05,
        finished_at: '2026-07-17T00:04:00.000000000Z',
      }),
    ]);
    expect(rows.map((r) => r.stage)).toEqual(['Splitting', 'Fact pass']);
    expect(rows[0]).toMatchObject({ model: '-', elapsed: '4s', failed: false, running: false });
    expect(rows[1]).toMatchObject({
      model: 'claude-opus',
      inTokens: 1200,
      outTokens: 800,
      cost: 0.05,
      elapsed: '4m',
    });
  });

  it('flags failed (ok:false) and running (ok:null) runs', () => {
    const rows = stageRunRows([
      run({ id: 1, ok: false }),
      run({ id: 2, ok: null, finished_at: '' }),
    ]);
    expect(rows[0]).toMatchObject({ failed: true, running: false });
    expect(rows[1]).toMatchObject({ failed: false, running: true, elapsed: '-' });
  });
});
