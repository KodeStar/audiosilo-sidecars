import { describe, it, expect } from 'vitest';
import {
  contributionChip,
  contributionRowLines,
  filterDoneBooks,
  runDurationSeconds,
  stageRunRows,
} from './doneBoard';
import type { BookView, ContributionRow, StageRun } from '@/api/types';

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

describe('contributionChip', () => {
  it('maps each aggregate status to its label, link, and tint', () => {
    expect(contributionChip({ status: 'submitted', url: 'https://x/i/1' })).toEqual({
      label: 'Issue open',
      url: 'https://x/i/1',
      attention: false,
    });
    expect(contributionChip({ status: 'pr_open', url: 'https://x/p/2' })).toEqual({
      label: 'PR open',
      url: 'https://x/p/2',
      attention: false,
    });
    expect(contributionChip({ status: 'merged', url: 'https://x/p/2' })).toEqual({
      label: 'Merged',
      url: 'https://x/p/2',
      attention: false,
    });
    expect(contributionChip({ status: 'closed', url: 'https://x/i/1' })).toEqual({
      label: 'Closed',
      url: 'https://x/i/1',
      attention: true,
    });
    expect(contributionChip({ status: 'local', url: '' })).toEqual({
      label: 'Local only',
      url: null,
      attention: false,
    });
  });

  it('renders an absent summary and an unknown status as the legacy Local only chip', () => {
    expect(contributionChip(undefined)).toEqual({
      label: 'Local only',
      url: null,
      attention: false,
    });
    expect(contributionChip({ status: 'weird', url: 'https://x' })).toEqual({
      label: 'Local only',
      url: null,
      attention: false,
    });
  });

  it('drops a link when the summary carries no url', () => {
    expect(contributionChip({ status: 'submitted', url: '' }).url).toBeNull();
  });
});

function crow(partial: Partial<ContributionRow>): ContributionRow {
  return {
    kind: 'characters',
    mode: 'issue',
    repo: 'KodeStar/audiosilo-meta',
    number: 0,
    url: '',
    pr_number: 0,
    pr_url: '',
    status: 'submitted',
    note: '',
    created_at: '',
    updated_at: '',
    ...partial,
  };
}

describe('contributionRowLines', () => {
  it('returns [] for absent rows', () => {
    expect(contributionRowLines(undefined)).toEqual([]);
  });

  it('labels each row with its kind + number, status word, and best link', () => {
    const lines = contributionRowLines([
      crow({ kind: 'characters', number: 123, url: 'https://x/i/123', status: 'submitted' }),
      crow({ kind: 'recaps', number: 124, url: '', pr_url: 'https://x/p/9', status: 'pr_open' }),
      crow({ kind: 'core', number: 0, status: 'merged', note: 'work merged' }),
    ]);
    expect(lines[0]).toEqual({
      key: 'characters',
      label: 'characters #123',
      statusLabel: 'issue open',
      url: 'https://x/i/123',
      note: '',
    });
    // Falls back to pr_url when the row has no direct url.
    expect(lines[1]).toMatchObject({
      label: 'recaps #124',
      url: 'https://x/p/9',
      statusLabel: 'PR open',
    });
    // No number -> no "#"; no link -> null; note carried.
    expect(lines[2]).toEqual({
      key: 'core',
      label: 'core',
      statusLabel: 'merged',
      url: null,
      note: 'work merged',
    });
  });
});
