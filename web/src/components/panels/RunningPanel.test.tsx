import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, act, waitFor } from '@testing-library/react';
import { RunningPanel } from './RunningPanel';
import type { ApiClient } from '@/lib/apiClient';
import type { BookDetail, BookView, SystemInfo } from '@/api/types';

// A controllable EventSource: jsdom has no EventSource, and the panel opens one
// via useEventStream. This stub captures the listeners so a test can emit SSE
// frames (book.state / eta.update / ...) synchronously.
type ESListener = (e: MessageEvent) => void;
let currentES: FakeEventSource | null = null;
function setCurrentES(es: FakeEventSource) {
  currentES = es;
}

class FakeEventSource {
  static readonly CLOSED = 2;
  readyState = 0;
  private listeners = new Map<string, Set<ESListener>>();
  constructor() {
    setCurrentES(this);
  }
  addEventListener(type: string, fn: ESListener) {
    let set = this.listeners.get(type);
    if (!set) {
      set = new Set();
      this.listeners.set(type, set);
    }
    set.add(fn);
  }
  removeEventListener(type: string, fn: ESListener) {
    this.listeners.get(type)?.delete(fn);
  }
  close() {}
  emit(type: string, data: unknown) {
    const evt = { data: JSON.stringify(data) } as MessageEvent;
    this.listeners.get(type)?.forEach((fn) => fn(evt));
  }
}

beforeEach(() => {
  currentES = null;
  vi.stubGlobal('EventSource', FakeEventSource);
});
afterEach(() => {
  vi.unstubAllGlobals();
});

function system(scratchBytes: number): SystemInfo {
  return {
    version: '1.0',
    data_dir: '/data',
    listen: '127.0.0.1:8090',
    tabs: [],
    tools: { ffmpeg: '/usr/bin/ffmpeg', ffprobe: '/usr/bin/ffprobe' },
    asr: {
      backend: 'mlx-whisper',
      available: true,
      device: 'metal',
      version: 'Python 3.12',
      detail: '',
    },
    agent: { backend: 'claude', available: true, version: '1.0.0' },
    scratch_bytes: scratchBytes,
  };
}

function bk(partial: Partial<BookView>): BookView {
  return {
    id: partial.id ?? 1,
    source_path: partial.source_path ?? '/x',
    title: partial.title ?? 'A Book',
    authors: partial.authors ?? [],
    state: partial.state ?? 'asr',
    lane: partial.lane ?? 'asr',
    status: partial.status ?? '',
    progress: partial.progress ?? [],
    scratch_bytes: partial.scratch_bytes ?? 0,
    total_cost_usd: partial.total_cost_usd ?? 0,
    created_at: partial.created_at ?? '2026-01-01T00:00:00Z',
    updated_at: partial.updated_at ?? '2026-01-01T00:00:00Z',
    ...partial,
  };
}

function detail(book: BookView): BookDetail {
  return { ...book, stage_runs: [] };
}

// fakeClient builds a minimal ApiClient with the methods the panel touches.
function fakeClient(books: BookView[], overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    listBooks: vi.fn().mockResolvedValue({ books }),
    system: vi.fn().mockResolvedValue(system(0)),
    getBook: vi.fn((id: number) =>
      Promise.resolve(detail(books.find((b) => b.id === id) ?? bk({}))),
    ),
    getBookEvents: vi.fn().mockResolvedValue({ events: [] }),
    ...overrides,
  } as unknown as ApiClient;
}

describe('RunningPanel scratch gauge', () => {
  it('renders the daemon-total scratch from /system in the header strip', async () => {
    const client = {
      listBooks: vi.fn().mockResolvedValue({ books: [] }),
      system: vi.fn().mockResolvedValue(system(1536)),
    } as unknown as ApiClient;

    render(<RunningPanel client={client} apiBase="" token="tok" />);

    // formatBytes(1536) === "1.5 KB", labelled Scratch.
    expect(await screen.findByText('1.5 KB')).toBeInTheDocument();
    expect(screen.getByText('Scratch')).toBeInTheDocument();
  });
});

describe('RunningPanel stage timeline', () => {
  it('renders the compact stage-chip timeline with the current stage active', async () => {
    const client = fakeClient([bk({ id: 1, state: 'asr', lane: 'asr' })]);
    render(<RunningPanel client={client} apiBase="" token="tok" />);

    await screen.findByText('A Book');
    // Compact timeline chips (distinct from the primary "Transcribing" state chip).
    expect(screen.getByText('ASR')).toBeInTheDocument(); // active chip
    expect(screen.getByText('Inspect')).toBeInTheDocument(); // done chip
    expect(screen.getByText('Facts')).toBeInTheDocument(); // pending chip
  });
});

describe('RunningPanel eta.update', () => {
  it('patches each row ETA and shows the queue ETA in the strip', async () => {
    const client = fakeClient([bk({ id: 1, state: 'asr' })]);
    render(<RunningPanel client={client} apiBase="" token="tok" />);
    await screen.findByText('A Book');

    act(() => {
      currentES?.emit('eta.update', {
        queue_seconds: 5400,
        books: [{ book_id: 1, eta_seconds: 2400 }],
      });
    });

    // Row ETA (formatEta(2400) === "~40m") and queue ETA (formatEta(5400) === "~1h 30m").
    expect(await screen.findByText('ETA ~40m')).toBeInTheDocument();
    expect(screen.getByText('Queue ETA')).toBeInTheDocument();
    expect(screen.getByText('~1h 30m')).toBeInTheDocument();
  });

  it('hides the queue ETA when a later eta.update reports a null makespan', async () => {
    const client = fakeClient([bk({ id: 1, state: 'asr' })]);
    render(<RunningPanel client={client} apiBase="" token="tok" />);
    await screen.findByText('A Book');

    act(() => {
      currentES?.emit('eta.update', { queue_seconds: 5400, books: [] });
    });
    expect(await screen.findByText('Queue ETA')).toBeInTheDocument();

    // Go idle: the daemon now sends queue_seconds null, which must hide the strip
    // stat (not read as 0).
    act(() => {
      currentES?.emit('eta.update', { queue_seconds: null, books: [] });
    });
    await waitFor(() => expect(screen.queryByText('Queue ETA')).not.toBeInTheDocument());
  });

  it('does not render an ETA chip for a non-running (paused) book', async () => {
    // A paused book can still carry a stale eta_seconds from before it parked; the
    // row must not advertise it until the next eta.update clears it.
    const client = fakeClient([bk({ id: 1, state: 'asr', status: 'paused', eta_seconds: 2400 })]);
    render(<RunningPanel client={client} apiBase="" token="tok" />);

    await screen.findByText('A Book');
    expect(screen.queryByText(/^ETA /)).not.toBeInTheDocument();
  });
});

describe('RunningPanel park hint', () => {
  it('shows the actionable hint for a parked book', async () => {
    const client = fakeClient([
      bk({
        id: 1,
        state: 'markers_normalizing',
        lane: 'agent',
        status: 'needs_attention',
        error: 'no confident markers',
        park_code: 'markers_not_confident',
      }),
    ]);
    render(<RunningPanel client={client} apiBase="" token="tok" />);

    await screen.findByText('A Book');
    expect(
      screen.getByText(
        "Chapter markers could not be normalized confidently. Check the audio's chapters, then Retry.",
      ),
    ).toBeInTheDocument();
  });
});

describe('RunningPanel elapsed clock', () => {
  it('shows elapsed for an active book with a start time and none for a done book', async () => {
    const client = fakeClient([
      bk({ id: 1, state: 'asr', started_at: new Date(Date.now() - 90_000).toISOString() }),
      bk({ id: 2, title: 'Finished Book', state: 'done', lane: '' }),
    ]);
    render(<RunningPanel client={client} apiBase="" token="tok" />);

    await screen.findByText('Finished Book');
    // The active row (with a start time) renders an elapsed readout; the done row
    // (which gets a stable clock and never renders elapsed) does not add one.
    expect(screen.getByText(/elapsed/)).toBeInTheDocument();
    expect(screen.getAllByText(/elapsed/)).toHaveLength(1);
  });
});

describe('RunningPanel live log', () => {
  it('renders the event log from getEvents when details are opened', async () => {
    const getBookEvents = vi.fn().mockResolvedValue({
      events: [
        {
          id: 2,
          ts: '2026-07-17T12:01:00Z',
          type: 'stage.progress',
          payload: { stage: 'asr', done: 3, total: 84 },
        },
        {
          id: 1,
          ts: '2026-07-17T12:00:00Z',
          type: 'book.state',
          payload: { state: 'asr', status: '' },
        },
      ],
    });
    const client = fakeClient([bk({ id: 1, state: 'asr' })], { getBookEvents });
    render(<RunningPanel client={client} apiBase="" token="tok" />);
    await screen.findByText('A Book');

    fireEvent.click(screen.getByRole('button', { name: 'Details' }));

    expect(await screen.findByText('asr 3/84')).toBeInTheDocument();
    expect(screen.getByText('state -> asr')).toBeInTheDocument();
    await waitFor(() => expect(getBookEvents).toHaveBeenCalledWith(1, 50));
  });
});
