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
  readonly url: string;
  private listeners = new Map<string, Set<ESListener>>();
  constructor(url: string) {
    this.url = url;
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
    listBooks: vi.fn().mockResolvedValue({ books, event_cursor: 0 }),
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

describe('RunningPanel supervisor', () => {
  it('shows status, split costs and recent decisions, and exposes manual diagnosis', async () => {
    const book = bk({ id: 7, batch_id: 'batch-7', title: 'Supervised Book' });
    const askSupervisor = vi.fn().mockResolvedValue({});
    const client = fakeClient([book], {
      supervisorStatus: vi.fn().mockResolvedValue({
        state: 'monitoring',
        enabled: true,
        automatic_actions: false,
        model_assisted: true,
        model_available: true,
        allow_backend_failover: false,
        runtime: { active_books: {}, agent_active: 1, agent_capacity: 2, eligible_agent_books: 1 },
      }),
      supervisorIncidents: vi.fn().mockResolvedValue({
        incidents: [
          {
            id: 1,
            batch_id: 'batch-7',
            trigger: 'health_tick',
            diagnosis: 'QA loop is not converging',
            confidence: 1,
            evidence: [],
            decision: 'qa_non_converging',
            selected_action: 'park_escalate',
            suggested_retry_limit: 3,
            suggested_termination_limit: 1,
            action_outcome: 'operator review',
            automatic: false,
            approval_required: true,
            state: 'approval_required',
            model_calls: 0,
            input_tokens: 0,
            output_tokens: 0,
            cached_tokens: 0,
            provider_cost_complete: true,
            estimate_complete: true,
            started_at: '2026-01-01T00:00:00Z',
          },
        ],
      }),
      supervisorCosts: vi.fn().mockResolvedValue({
        batch_id: 'batch-7',
        production_reported_usd: 12,
        production_reported_incomplete: false,
        production_estimated_api_usd: 13,
        production_estimate_incomplete: false,
        book_supervisor_reported_usd: 0.2,
        book_supervisor_estimated_api_usd: 0.3,
        batch_supervisor_reported_usd: 0.1,
        batch_supervisor_estimated_api_usd: 0.1,
        supervisor_reported_incomplete: false,
        supervisor_estimate_incomplete: false,
        overall_reported_usd: 12.3,
        overall_reported_incomplete: false,
        overall_estimated_api_usd: 13.4,
        overall_estimate_incomplete: false,
      }),
      askSupervisor,
    });
    render(<RunningPanel client={client} apiBase="" token="tok" />);

    expect(await screen.findByText('Supervisor: monitoring')).toBeInTheDocument();
    expect(screen.getByText(/production \$12\.0000 reported/)).toBeInTheDocument();
    expect(screen.getByText(/book supervision \$0\.2000 reported.*\$0\.3000/)).toBeInTheDocument();
    expect(screen.getByText(/batch supervision \$0\.1000 reported.*\$0\.1000/)).toBeInTheDocument();
    expect(screen.getByText(/QA loop is not converging.*park_escalate/)).toBeInTheDocument();

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: /ask supervisor/i }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    await waitFor(() => expect(askSupervisor).toHaveBeenCalledWith(7));
  });

  it('loads the newest active batch instead of an older legacy book', async () => {
    const incidents = vi.fn().mockResolvedValue({ incidents: [] });
    const costs = vi.fn().mockResolvedValue(null);
    const client = fakeClient(
      [
        bk({ id: 1, batch_id: 'legacy', state: 'done', lane: '' }),
        bk({ id: 9, batch_id: 'batch-current', state: 'fact_pass', lane: 'agent' }),
      ],
      {
        supervisorStatus: vi.fn().mockResolvedValue({
          state: 'monitoring',
          enabled: true,
          automatic_actions: false,
          model_assisted: false,
          model_available: false,
          allow_backend_failover: false,
          runtime: {
            active_books: {},
            agent_active: 0,
            agent_capacity: 2,
            eligible_agent_books: 1,
          },
        }),
        supervisorIncidents: incidents,
        supervisorCosts: costs,
      },
    );
    render(<RunningPanel client={client} apiBase="" token="tok" />);
    await waitFor(() => expect(incidents).toHaveBeenCalledWith('batch-current', 8));
    expect(costs).toHaveBeenCalledWith('batch-current');
  });

  it('shows the parked book supervisor diagnosis, action outcome, and evidence', async () => {
    const book = bk({
      id: 4,
      batch_id: 'legacy',
      title: 'Matched Saga Two',
      state: 'auditing',
      lane: 'agent',
      status: 'needs_attention',
      park_code: 'supervisor_escalated',
      error: 'supervisor parked this book for operator review',
    });
    const client = fakeClient([book], {
      supervisorStatus: vi.fn().mockResolvedValue({
        state: 'monitoring',
        enabled: true,
        automatic_actions: true,
        model_assisted: false,
        model_available: false,
        allow_backend_failover: false,
        runtime: { active_books: {}, agent_active: 0, agent_capacity: 2, eligible_agent_books: 0 },
      }),
      supervisorIncidents: vi.fn().mockResolvedValue({
        incidents: [
          {
            id: 11,
            batch_id: 'legacy',
            book_id: 4,
            stage_run_id: 408,
            trigger: 'health_tick',
            diagnosis: 'required artifact or completion sentinel is missing or invalid',
            confidence: 1,
            evidence: ['_done/auditing.json', 'no such file or directory'],
            decision: 'artifact_invalid',
            selected_action: 'park_escalate',
            suggested_retry_limit: 3,
            suggested_termination_limit: 1,
            action_outcome: 'book parked and escalated',
            automatic: true,
            approval_required: true,
            state: 'completed',
            model_calls: 0,
            input_tokens: 0,
            output_tokens: 0,
            cached_tokens: 0,
            provider_cost_complete: false,
            estimate_complete: false,
            started_at: '2026-07-19T09:52:42Z',
          },
        ],
      }),
      supervisorCosts: vi.fn().mockResolvedValue(null),
    });

    render(<RunningPanel client={client} apiBase="" token="tok" />);

    expect(await screen.findByText('Supervisor diagnosis:')).toBeInTheDocument();
    expect(
      screen.getByText('required artifact or completion sentinel is missing or invalid'),
    ).toBeInTheDocument();
    expect(
      screen.getByText('Action: park_escalate — book parked and escalated'),
    ).toBeInTheDocument();
    expect(screen.getByText('_done/auditing.json')).toBeInTheDocument();
    expect(screen.getByText('no such file or directory')).toBeInTheDocument();
  });

  it('does not offer manual diagnosis when the configured model is unavailable', async () => {
    const client = fakeClient([bk({ id: 4, batch_id: 'batch-4' })], {
      supervisorStatus: vi.fn().mockResolvedValue({
        state: 'monitoring',
        enabled: true,
        automatic_actions: false,
        model_assisted: true,
        model_available: false,
        allow_backend_failover: false,
        runtime: { active_books: {}, agent_active: 0, agent_capacity: 2, eligible_agent_books: 0 },
      }),
      supervisorIncidents: vi.fn().mockResolvedValue({ incidents: [] }),
      supervisorCosts: vi.fn().mockResolvedValue(null),
    });
    render(<RunningPanel client={client} apiBase="" token="tok" />);
    expect(await screen.findByText(/model unavailable/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /ask supervisor/i })).not.toBeInTheDocument();
  });

  it('coalesces supervisor decision bursts without reloading books', async () => {
    const status = vi.fn().mockResolvedValue({
      state: 'monitoring',
      enabled: true,
      automatic_actions: false,
      model_assisted: false,
      model_available: false,
      allow_backend_failover: false,
      runtime: { active_books: {}, agent_active: 0, agent_capacity: 2, eligible_agent_books: 1 },
    });
    const incidents = vi.fn().mockResolvedValue({ incidents: [] });
    const costs = vi.fn().mockResolvedValue(null);
    const client = fakeClient([bk({ id: 3, batch_id: 'batch-3' })], {
      supervisorStatus: status,
      supervisorIncidents: incidents,
      supervisorCosts: costs,
    });
    render(<RunningPanel client={client} apiBase="" token="tok" />);
    await waitFor(() => expect(status).toHaveBeenCalledTimes(1));
    vi.mocked(client.listBooks).mockClear();
    status.mockClear();
    incidents.mockClear();
    costs.mockClear();

    const run = {
      id: 10,
      batch_id: 'batch-3',
      trigger: 'health_tick',
      diagnosis: 'burst',
      confidence: 1,
      evidence: [],
      decision: 'rate_limit',
      selected_action: 'readmit',
      suggested_retry_limit: 1,
      suggested_termination_limit: 0,
      action_outcome: 'recorded',
      automatic: false,
      approval_required: false,
      state: 'decided',
      model_calls: 0,
      input_tokens: 0,
      output_tokens: 0,
      cached_tokens: 0,
      provider_cost_complete: true,
      estimate_complete: true,
      started_at: '2026-01-01T00:00:00Z',
    };
    act(() => {
      currentES?.emit('supervisor.decision', run);
      currentES?.emit('supervisor.decision', { ...run, id: 11 });
      currentES?.emit('supervisor.decision', { ...run, id: 12 });
    });

    await waitFor(() => expect(status).toHaveBeenCalledTimes(1));
    expect(client.listBooks).not.toHaveBeenCalled();
    expect(incidents).toHaveBeenCalledTimes(1);
    expect(costs).toHaveBeenCalledTimes(1);
  });
});

describe('RunningPanel stage timeline', () => {
  it('renders the compact stage-chip timeline with the current stage active', async () => {
    const client = fakeClient([bk({ id: 1, state: 'asr', lane: 'asr' })]);
    render(<RunningPanel client={client} apiBase="" token="tok" />);

    await screen.findByText('A Book');
    // Compact timeline chips (distinct from the primary "Transcribing" state chip).
    expect(screen.getByTitle('Transcribing')).toHaveTextContent('ASR'); // active chip
    expect(screen.getByText('Inspect')).toBeInTheDocument(); // done chip
    expect(screen.getByText('Facts')).toBeInTheDocument(); // pending chip
  });
});

describe('RunningPanel queue organization', () => {
  it('splits Processing from ASR and renders current workers before scheduler order', async () => {
    const client = fakeClient([
      bk({
        id: 4,
        title: 'ASR Next',
        queue_group: 'asr',
        queue_bucket: 'transcription',
        queue_position: 1,
        queue_active: false,
      }),
      bk({
        id: 2,
        title: 'Processing Next',
        state: 'spelling_research',
        lane: 'agent',
        queue_group: 'processing',
        queue_bucket: 'agent',
        queue_position: 1,
        queue_active: false,
      }),
      bk({
        id: 3,
        title: 'ASR Current',
        queue_group: 'asr',
        queue_bucket: 'transcribing',
        queue_position: 1,
        queue_active: true,
      }),
      bk({
        id: 1,
        title: 'Processing Current',
        state: 'fact_pass',
        lane: 'agent',
        queue_group: 'processing',
        queue_bucket: 'agent_active',
        queue_position: 1,
        queue_active: true,
      }),
    ]);
    render(<RunningPanel client={client} apiBase="" token="tok" />);

    await screen.findByText('Processing Current');
    expect(screen.getByRole('heading', { name: 'Processing' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'ASR' })).toBeInTheDocument();
    expect(screen.getByText('Agent processing now')).toBeInTheDocument();
    expect(screen.getByText('Transcribing now')).toBeInTheDocument();
    expect(screen.getByText('Agent queue')).toBeInTheDocument();
    expect(screen.getByText('Transcription queue')).toBeInTheDocument();

    const titles = ['Processing Current', 'Processing Next', 'ASR Current', 'ASR Next'].map(
      (title) => screen.getByText(title),
    );
    for (let i = 1; i < titles.length; i += 1) {
      expect(
        titles[i - 1].compareDocumentPosition(titles[i]) & Node.DOCUMENT_POSITION_FOLLOWING,
      ).toBeTruthy();
    }
  });

  it('uses the REST cursor for a lossless SSE handoff and applies live queue handoffs', async () => {
    let resolveList!: (value: Awaited<ReturnType<ApiClient['listBooks']>>) => void;
    const list = new Promise<Awaited<ReturnType<ApiClient['listBooks']>>>((resolve) => {
      resolveList = resolve;
    });
    const first = bk({
      id: 1,
      title: 'First ASR',
      queue_group: 'asr',
      queue_bucket: 'transcribing',
      queue_position: 1,
      queue_active: true,
    });
    const second = bk({
      id: 2,
      title: 'Second ASR',
      queue_group: 'asr',
      queue_bucket: 'transcription',
      queue_position: 1,
      queue_active: false,
    });
    const client = fakeClient([], { listBooks: vi.fn().mockReturnValue(list) });

    render(<RunningPanel client={client} apiBase="/sidecars" token="tok" />);
    expect(currentES).toBeNull();

    await act(async () => resolveList({ books: [first, second], event_cursor: 42 }));
    await screen.findByText('First ASR');
    await waitFor(() => expect(currentES).not.toBeNull());
    expect(currentES?.url).toContain('lastEventId=42');

    act(() => {
      currentES?.emit('queue.stats', {
        asr_active: 1,
        agent_active: 0,
        mechanical_active: 0,
        queued: 2,
        queue_books: [
          { book_id: 2, group: 'asr', bucket: 'transcribing', position: 1, active: true },
          { book_id: 1, group: 'asr', bucket: 'transcription', position: 1, active: false },
        ],
      });
    });
    const handedOff = [screen.getByText('Second ASR'), screen.getByText('First ASR')];
    expect(
      handedOff[0].compareDocumentPosition(handedOff[1]) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(screen.getByText('Runnable')).toBeInTheDocument();

    act(() => {
      currentES?.emit('queue.stats', {
        asr_active: 0,
        agent_active: 0,
        mechanical_active: 0,
        queued: 2,
        queue_books: [
          { book_id: 1, group: 'asr', bucket: 'transcription', position: 1, active: false },
        ],
      });
    });
    expect(screen.getByText('Up next')).toBeInTheDocument();
  });
});

describe('RunningPanel series waiting state', () => {
  it('shows the scheduler blocker and clears it from a live queue update', async () => {
    const client = fakeClient([
      bk({
        id: 1,
        title: 'Matched Saga One',
        series: 'Matched Saga',
        series_pos: '1',
        state: 'fact_pass',
        lane: 'agent',
        active_agent_invocations: 1,
      }),
      bk({
        id: 2,
        title: 'Matched Saga Two',
        series: 'Matched Saga',
        series_pos: '2',
        state: 'spelling_research',
        lane: 'agent',
        series_blocked_by: { book_id: 1, title: 'Matched Saga One', series_pos: '1' },
      }),
    ]);
    render(<RunningPanel client={client} apiBase="" token="tok" />);

    expect(
      await screen.findByText('Waiting for earlier series book: Matched Saga One #1'),
    ).toBeInTheDocument();

    act(() => {
      currentES?.emit('queue.stats', {
        asr_active: 0,
        agent_active: 1,
        agent_invocations_by_book: { '2': 1 },
        series_blocked_by: {},
        mechanical_active: 0,
        queued: 1,
      });
    });

    await waitFor(() =>
      expect(
        screen.queryByText('Waiting for earlier series book: Matched Saga One #1'),
      ).not.toBeInTheDocument(),
    );
    expect(screen.getByText('1 active agent invocation')).toBeInTheDocument();
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
    await waitFor(() => expect(getBookEvents).toHaveBeenCalledWith(1, 50, undefined));
  });
});
