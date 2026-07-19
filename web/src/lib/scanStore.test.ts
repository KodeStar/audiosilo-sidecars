import { describe, it, expect, vi } from 'vitest';
import { ApiError, type ApiClient } from '@/lib/apiClient';
import type { Coverage, ScanJob, ScannedBook } from '@/api/types';
import {
  mergeBooks,
  overrideErrorMessage,
  scanErrorMessage,
  ScanStore,
  type BookPatch,
} from './scanStore';

// Flush pending microtasks by yielding a macrotask turn (a real timer - the store
// uses an INJECTED timer for its poll loop, so this does not fire scan polls).
const flush = () => new Promise<void>((r) => setTimeout(r, 0));

function cov(partial: Partial<Coverage> = {}): Coverage {
  return { available: true, known: true, has_characters: false, has_recaps: false, ...partial };
}

function scannedBook(partial: Partial<ScannedBook>): ScannedBook {
  return {
    path: partial.path ?? '/x',
    source_path: partial.source_path ?? '/root' + (partial.path ?? '/x'),
    title: partial.title ?? 'T',
    audio_files: 1,
    coverage: partial.coverage ?? cov(),
    ...partial,
  };
}

function job(partial: Partial<ScanJob>): ScanJob {
  return {
    id: partial.id ?? 'j1',
    path: partial.path ?? '/root',
    status: partial.status ?? 'running',
    progress: partial.progress ?? {
      phase: 'scanning',
      walk_dirs: 0,
      walk_groups: 0,
      groups_done: 0,
      groups_total: 0,
      books_found: 0,
      coverage_done: 0,
      coverage_total: 0,
    },
    books: partial.books ?? [],
    ...partial,
  };
}

// A test store whose poll timer is manual: setTimeoutFn stashes the next tick so a
// test fires it explicitly, and a counter proves the loop stopped at a terminal
// status.
function makeStore() {
  let scheduled: (() => void) | null = null;
  let scheduleCount = 0;
  const store = new ScanStore({
    pollMs: 5,
    setTimeoutFn: (cb) => {
      scheduled = cb;
      scheduleCount++;
      return 1 as unknown as ReturnType<typeof setTimeout>;
    },
    clearTimeoutFn: () => {
      scheduled = null;
    },
  });
  return {
    store,
    fireNextPoll: () => scheduled?.(),
    scheduleCount: () => scheduleCount,
  };
}

describe('mergeBooks', () => {
  const a = scannedBook({ path: '/a' });
  const b = scannedBook({ path: '/b' });

  it('returns the same array reference when there are no patches', () => {
    const patches = new Map<string, BookPatch>();
    const input = [a, b];
    expect(mergeBooks(input, patches)).toBe(input);
  });

  it('applies hidden + coverage patches, preserving order and untouched books', () => {
    const patches = new Map<string, BookPatch>([
      [a.source_path, { hidden: true }],
      [b.source_path, { coverage: cov({ matched_by: 'manual', work_id: 'w1' }) }],
    ]);
    const out = mergeBooks([a, b], patches);
    expect(out[0].hidden).toBe(true);
    expect(out[1].coverage.matched_by).toBe('manual');
    // A path with no patch is passed through by reference.
    const c = scannedBook({ path: '/c' });
    expect(mergeBooks([c], patches)[0]).toBe(c);
  });

  it('applies matched series metadata to a manually matched scan row', () => {
    const patches = new Map<string, BookPatch>([
      [
        a.source_path,
        {
          coverage: cov({
            matched_by: 'manual',
            work_id: 'work-a',
            series: { name: 'Matched Saga', position: '5' },
          }),
        },
      ],
    ]);

    const [matched] = mergeBooks([a], patches);

    expect(matched.series).toBe('Matched Saga');
    expect(matched.series_position).toBe('5');
    expect(matched.sources?.series).toBe('metadata');
    expect(matched.sources?.series_position).toBe('metadata');
  });
});

describe('error helpers', () => {
  it('scanErrorMessage spells out the 403 library-roots case', () => {
    expect(scanErrorMessage(new ApiError(403, 'x'))).toMatch(/library_roots/);
    expect(scanErrorMessage(new ApiError(500, 'boom'))).toBe('boom');
    expect(scanErrorMessage(new Error('net'))).toBe('Could not reach the daemon.');
    expect(scanErrorMessage(new Error('net'), 'other')).toBe('other');
  });

  it('overrideErrorMessage maps 403 and passes messages through', () => {
    expect(overrideErrorMessage(new ApiError(403, 'x'))).toMatch(/library roots/);
    expect(overrideErrorMessage(new ApiError(404, 'no work'))).toBe('no work');
    expect(overrideErrorMessage('weird')).toBe('Could not update the book.');
  });
});

describe('ScanStore scan lifecycle', () => {
  it('starts a scan, polls, applies books, and stops on done', async () => {
    const { store, fireNextPoll, scheduleCount } = makeStore();
    const b1 = scannedBook({ path: '/b1' });
    const b2 = scannedBook({ path: '/b2' });
    const client = {
      createScan: vi.fn().mockResolvedValue({ job_id: 'j1' }),
      getScan: vi
        .fn()
        .mockResolvedValueOnce(job({ status: 'running', books: [b1] }))
        .mockResolvedValueOnce(job({ status: 'done', books: [b1, b2] })),
    } as unknown as ApiClient;

    const outcome = await store.startScan(client, '/root');
    await flush();

    expect(outcome).toEqual({ ok: true }); // createScan was accepted
    expect(store.getSnapshot().starting).toBe(false);
    expect(store.getSnapshot().job?.status).toBe('running');
    expect(store.getSnapshot().job?.books).toHaveLength(1);
    expect(scheduleCount()).toBe(1); // a next poll was scheduled

    fireNextPoll();
    await flush();

    expect(store.getSnapshot().job?.status).toBe('done');
    expect(store.getSnapshot().job?.books).toHaveLength(2);
    expect(scheduleCount()).toBe(1); // no further poll after a terminal status
  });

  it('surfaces a 403 from createScan as a friendly scan error', async () => {
    const { store } = makeStore();
    const client = {
      createScan: vi.fn().mockRejectedValue(new ApiError(403, 'nope')),
    } as unknown as ApiClient;

    const outcome = await store.startScan(client, '/root');

    expect(outcome).toEqual({ ok: false }); // rejected -> caller must not record the root
    expect(store.getSnapshot().starting).toBe(false);
    expect(store.getSnapshot().scanError).toMatch(/library_roots/);
  });

  it('surfaces a poll failure as a scan error', async () => {
    const { store } = makeStore();
    const client = {
      createScan: vi.fn().mockResolvedValue({ job_id: 'j1' }),
      getScan: vi.fn().mockRejectedValue(new ApiError(500, 'read failed')),
    } as unknown as ApiClient;

    await store.startScan(client, '/root');
    await flush();

    expect(store.getSnapshot().scanError).toBe('read failed');
  });

  it('is re-entrant: a second startScan supersedes the first poll', async () => {
    const { store, scheduleCount } = makeStore();
    const client = {
      createScan: vi.fn().mockResolvedValue({ job_id: 'j1' }),
      getScan: vi.fn().mockResolvedValue(job({ status: 'running', books: [] })),
    } as unknown as ApiClient;

    await store.startScan(client, '/a');
    await flush();
    const first = scheduleCount();
    await store.startScan(client, '/b');
    await flush();

    // The store did not accumulate two live poll loops per fired tick; each scan
    // scheduled its own single next-tick.
    expect(scheduleCount()).toBe(first + 1);
    expect(store.getSnapshot().job?.status).toBe('running');
  });
});

describe('ScanStore reattach', () => {
  it('adopts the newest scan and fetches its books', async () => {
    const { store } = makeStore();
    const b1 = scannedBook({ path: '/b1' });
    const client = {
      listScans: vi.fn().mockResolvedValue({
        scans: [{ id: 'jZ', path: '/root', status: 'done', progress: job({}).progress }],
      }),
      getScan: vi.fn().mockResolvedValue(job({ id: 'jZ', status: 'done', books: [b1] })),
    } as unknown as ApiClient;

    await store.reattach(client);
    await flush();

    expect(store.getSnapshot().job?.id).toBe('jZ');
    expect(store.getSnapshot().job?.books).toHaveLength(1);
  });

  it('runs at most once per session', async () => {
    const { store } = makeStore();
    const client = {
      listScans: vi.fn().mockResolvedValue({ scans: [] }),
    } as unknown as ApiClient;

    await store.reattach(client);
    await store.reattach(client);

    expect((client.listScans as ReturnType<typeof vi.fn>).mock.calls).toHaveLength(1);
  });

  it('does nothing when there is no prior scan', async () => {
    const { store } = makeStore();
    const client = {
      listScans: vi.fn().mockResolvedValue({ scans: [] }),
    } as unknown as ApiClient;

    await store.reattach(client);
    expect(store.getSnapshot().job).toBeNull();
  });

  it('retries after a transient failure (the latch resets on error)', async () => {
    const { store } = makeStore();
    const b1 = scannedBook({ path: '/b1' });
    const listScans = vi
      .fn()
      .mockRejectedValueOnce(new ApiError(503, 'daemon busy'))
      .mockResolvedValueOnce({
        scans: [{ id: 'jZ', path: '/root', status: 'done', progress: job({}).progress }],
      });
    const client = {
      listScans,
      getScan: vi.fn().mockResolvedValue(job({ id: 'jZ', status: 'done', books: [b1] })),
    } as unknown as ApiClient;

    // First reattach rejects and leaves the tab empty...
    await store.reattach(client);
    await flush();
    expect(store.getSnapshot().job).toBeNull();

    // ...but a later mount retries (the latch was reset) and adopts the scan.
    await store.reattach(client);
    await flush();
    expect(listScans.mock.calls).toHaveLength(2);
    expect(store.getSnapshot().job?.id).toBe('jZ');
  });
});

describe('ScanStore reset', () => {
  it('returns to INITIAL, keeps no preferences, and re-arms reattach', async () => {
    const { store } = makeStore();
    const client = {
      createScan: vi.fn().mockResolvedValue({ job_id: 'j1' }),
      getScan: vi.fn().mockResolvedValue(job({ status: 'done', books: [scannedBook({})] })),
      listScans: vi.fn().mockResolvedValue({ scans: [] }),
    } as unknown as ApiClient;

    store.setExcludeCovered(true);
    store.setSearch('dune');
    await store.startScan(client, '/root');
    await flush();
    // The once-per-session reattach latch is now set.
    await store.reattach(client);
    expect((client.listScans as ReturnType<typeof vi.fn>).mock.calls).toHaveLength(0);

    store.reset();

    // State is back to INITIAL (job cleared, preferences reset).
    expect(store.getSnapshot().job).toBeNull();
    expect(store.getSnapshot().excludeCovered).toBe(false);
    expect(store.getSnapshot().search).toBe('');

    // reattach works again after a reset (the latch was re-armed).
    await store.reattach(client);
    expect((client.listScans as ReturnType<typeof vi.fn>).mock.calls).toHaveLength(1);
  });
});

describe('ScanStore overrides', () => {
  async function withDoneScan() {
    const { store, fireNextPoll } = makeStore();
    const b1 = scannedBook({ path: '/b1' });
    const client = {
      createScan: vi.fn().mockResolvedValue({ job_id: 'j1' }),
      getScan: vi.fn().mockResolvedValue(job({ status: 'done', books: [b1] })),
      setOverride: vi.fn(),
    } as unknown as ApiClient;
    await store.startScan(client, '/root');
    await flush();
    return { store, client, b1, fireNextPoll };
  }

  it('hide sets the overlay hidden flag and deselects the book', async () => {
    const { store, client, b1 } = await withDoneScan();
    store.toggleOne('/b1', true);
    expect(store.getSnapshot().selected.has('/b1')).toBe(true);

    (client.setOverride as ReturnType<typeof vi.fn>).mockResolvedValue({
      override: {
        source_path: '/root/b1',
        hidden: true,
        work_id: '',
        work_title: '',
        updated_at: '',
      },
      coverage: null,
    });

    const res = await store.hide(client, b1);
    expect(res.ok).toBe(true);
    expect(store.getSnapshot().job?.books[0].hidden).toBe(true);
    expect(store.getSnapshot().selected.has('/b1')).toBe(false);
  });

  it('applyManualMatch patches the coverage from the response', async () => {
    const { store, client, b1 } = await withDoneScan();
    (client.setOverride as ReturnType<typeof vi.fn>).mockResolvedValue({
      override: {
        source_path: '/root/b1',
        hidden: false,
        work_id: 'w1',
        work_title: 'Dune',
        updated_at: '',
      },
      coverage: cov({ matched_by: 'manual', work_id: 'w1', work_title: 'Dune' }),
    });

    await store.applyManualMatch(client, b1, 'w1');

    const patched = store.getSnapshot().job?.books[0].coverage;
    expect(patched?.matched_by).toBe('manual');
    expect(patched?.work_title).toBe('Dune');
    // The POST carried the full desired state.
    const body = (client.setOverride as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body).toEqual({ source_path: '/root/b1', hidden: false, work_id: 'w1' });
  });

  it('clearMatch reverts coverage to unknown', async () => {
    const { store, client } = await withDoneScan();
    const matched = scannedBook({
      path: '/b1',
      coverage: cov({ matched_by: 'manual', work_id: 'w1' }),
    });
    (client.setOverride as ReturnType<typeof vi.fn>).mockResolvedValue({
      override: {
        source_path: '/root/b1',
        hidden: false,
        work_id: '',
        work_title: '',
        updated_at: '',
      },
      coverage: null,
    });

    await store.clearMatch(client, matched);

    const cleared = store.getSnapshot().job?.books[0].coverage;
    expect(cleared?.known).toBe(false);
    expect(cleared?.matched_by).toBeUndefined();
  });

  it('returns a friendly error when an override fails', async () => {
    const { store, client, b1 } = await withDoneScan();
    (client.setOverride as ReturnType<typeof vi.fn>).mockRejectedValue(
      new ApiError(404, 'no work'),
    );

    const res = await store.hide(client, b1);
    expect(res).toEqual({ ok: false, error: 'no work' });
  });
});

describe('ScanStore process', () => {
  it('reports started + clears selection when a book was created', async () => {
    const { store } = makeStore();
    const client = {
      createBooks: vi.fn().mockResolvedValue({ results: [{ source_path: '/b1', created: true }] }),
    } as unknown as ApiClient;
    store.toggleOne('/b1', true);

    const outcome = await store.process(client, [
      {
        source_path: '/b1',
        title: 'x',
        authors: [],
        series: '',
        series_pos: '',
        asin: '',
        isbn: '',
      },
    ]);

    expect(outcome).toEqual({ started: true });
    expect(store.getSnapshot().selected.size).toBe(0);
    expect(store.getSnapshot().processing).toBe(false);
  });

  it('sets an error note for conflicts and does not report started', async () => {
    const { store } = makeStore();
    const client = {
      createBooks: vi
        .fn()
        .mockResolvedValue({ results: [{ source_path: '/b1', created: false, conflict: true }] }),
    } as unknown as ApiClient;

    const outcome = await store.process(client, [
      {
        source_path: '/b1',
        title: 'x',
        authors: [],
        series: '',
        series_pos: '',
        asin: '',
        isbn: '',
      },
    ]);

    expect(outcome).toEqual({ started: false });
    expect(store.getSnapshot().note).toEqual({ kind: 'error', text: '1 already enqueued.' });
  });

  it('immediately marks a newly created scan row as tracked', async () => {
    const { store } = makeStore();
    const b1 = scannedBook({ path: '/b1', source_path: '/root/b1' });
    const client = {
      createScan: vi.fn().mockResolvedValue({ job_id: 'j1' }),
      getScan: vi.fn().mockResolvedValue(job({ status: 'done', books: [b1] })),
      createBooks: vi.fn().mockResolvedValue({
        results: [
          {
            source_path: '/root/b1',
            created: true,
            book: { id: 41, state: 'queued', status: '' },
          },
        ],
      }),
    } as unknown as ApiClient;
    await store.startScan(client, '/root');
    await flush();

    await store.process(client, [
      {
        source_path: '/root/b1',
        title: 'x',
        authors: [],
        series: '',
        series_pos: '',
        asin: '',
        isbn: '',
      },
    ]);

    expect(store.getSnapshot().job?.books[0].pipeline_book).toEqual({
      id: 41,
      state: 'queued',
      status: '',
    });
    store.toggleOne('/b1', true);
    expect(store.getSnapshot().selected.has('/b1')).toBe(false);
  });
});

describe('ScanStore selection + preferences', () => {
  it('toggles single + all and clears', () => {
    const { store } = makeStore();
    store.toggleOne('/a', true);
    store.toggleOne('/b', true);
    expect(store.getSnapshot().selected.size).toBe(2);
    store.toggleOne('/a', false);
    expect(store.getSnapshot().selected.has('/a')).toBe(false);

    store.toggleAll(['/a', '/c'], true);
    expect(store.getSnapshot().selected.has('/a')).toBe(true);
    expect(store.getSnapshot().selected.has('/c')).toBe(true);
    store.toggleAll(['/a', '/c'], false);
    expect(store.getSnapshot().selected.has('/c')).toBe(false);
  });

  it('tracks the toggle preferences', () => {
    const { store } = makeStore();
    store.setExcludeCovered(true);
    store.setShowHidden(true);
    expect(store.getSnapshot().excludeCovered).toBe(true);
    expect(store.getSnapshot().showHidden).toBe(true);
  });

  it('tracks the search query and defaults to empty', () => {
    const { store } = makeStore();
    expect(store.getSnapshot().search).toBe('');
    store.setSearch('dune');
    expect(store.getSnapshot().search).toBe('dune');
  });

  it('clearForNewScan resets the scan state but keeps preferences', async () => {
    const { store } = makeStore();
    const client = {
      createScan: vi.fn().mockResolvedValue({ job_id: 'j1' }),
      getScan: vi.fn().mockResolvedValue(job({ status: 'done', books: [scannedBook({})] })),
    } as unknown as ApiClient;
    store.setExcludeCovered(true);
    await store.startScan(client, '/root');
    await flush();
    expect(store.getSnapshot().job).not.toBeNull();

    store.clearForNewScan();
    expect(store.getSnapshot().job).toBeNull();
    expect(store.getSnapshot().excludeCovered).toBe(true);
  });
});
