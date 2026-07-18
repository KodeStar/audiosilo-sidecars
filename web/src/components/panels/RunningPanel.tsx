import { useCallback, useEffect, useRef, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type {
  BookStateEvent,
  BookView,
  ContributionUpdateEvent,
  EtaUpdateEvent,
  QueueStatsEvent,
  StageNoteEvent,
  StageProgressEvent,
  SupervisorStatus,
  SupervisorRun,
  BatchCostSummary,
} from '@/api/types';
import {
  applyBookState,
  applyEtaUpdate,
  applyStageProgress,
  bumpBookEventCount,
  formatBytes,
  isDone,
  pruneBookEventCounts,
  sortBooks,
  type BookAction,
} from '@/lib/books';
import { formatEta } from '@/lib/duration';
import { formatCost } from '@/lib/cost';
import { useEventStream, type PipelineEventType } from '@/lib/useEventStream';
import { BookRow } from '../running/BookRow';
import { CoreProposalModal } from '../running/CoreProposalModal';

// The elapsed clock is coarse (rows show whole minutes/seconds), so a 30s tick is
// plenty and keeps the panel cheap.
const CLOCK_TICK_MS = 30_000;
const SUPERVISOR_REFRESH_DEBOUNCE_MS = 100;

function newestRelevantBatch(books: BookView[]): string | undefined {
  const active = books.filter((book) => !isDone(book));
  const candidates = active.length > 0 ? active : books;
  return candidates.reduce<BookView | undefined>(
    (newest, book) => (!newest || book.id > newest.id ? book : newest),
    undefined,
  )?.batch_id;
}

interface RunningPanelProps {
  client: ApiClient;
  apiBase: string;
  token: string;
}

export function RunningPanel({ client, apiBase, token }: RunningPanelProps) {
  const [books, setBooks] = useState<BookView[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [stats, setStats] = useState<QueueStatsEvent | null>(null);
  const [queueSeconds, setQueueSeconds] = useState<number | null>(null);
  const [scratchBytes, setScratchBytes] = useState<number | null>(null);
  const [busyId, setBusyId] = useState<number | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [supervisorStatus, setSupervisorStatus] = useState<SupervisorStatus | null>(null);
  const [incidents, setIncidents] = useState<SupervisorRun[]>([]);
  const [batchCosts, setBatchCosts] = useState<BatchCostSummary | null>(null);
  const supervisorRefreshTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // The book whose core (add-work) proposal modal is open, or null when closed.
  const [coreBook, setCoreBook] = useState<BookView | null>(null);
  // A single shared clock driving every row's elapsed display (no per-row timer).
  const [now, setNow] = useState(() => Date.now());
  // Per-book counter of durable book-scoped SSE frames, giving BookRow a changing
  // prop that retriggers its throttled log refetch. See bumpBookEventCount.
  const [bookEventCounts, setBookEventCounts] = useState<Record<number, number>>({});

  const refreshSupervisor = useCallback(
    async (batchId?: string) => {
      if (typeof client.supervisorStatus !== 'function') return;
      const [status, recent, costs] = await Promise.all([
        client.supervisorStatus(),
        client.supervisorIncidents(batchId, 8),
        batchId ? client.supervisorCosts(batchId) : Promise.resolve(null),
      ]);
      setSupervisorStatus(status);
      setIncidents(recent.incidents);
      setBatchCosts(costs);
    },
    [client],
  );

  // Reload the books AND the daemon-total scratch gauge together. Fetching the
  // gauge here (not just on mount) keeps it fresh after an action - notably a
  // purge, which drops the total. The two calls are independent, so a gauge
  // failure never blocks the pipeline list.
  const load = useCallback(async () => {
    try {
      const { books: list } = await client.listBooks();
      setBooks(sortBooks(list));
      // Drop counters for books the reload no longer holds (functional setState so a
      // concurrent bump is not clobbered).
      const liveIds = new Set(list.map((b) => b.id));
      setBookEventCounts((prev) => pruneBookEventCounts(prev, liveIds));
      setLoadError(null);
      void refreshSupervisor(newestRelevantBatch(list)).catch(() => undefined);
    } catch (err) {
      setLoadError(err instanceof ApiError ? err.message : 'Could not load the pipeline.');
    }
    try {
      const info = await client.system();
      setScratchBytes(info.scratch_bytes);
    } catch {
      // A gauge read failure is non-fatal; leave the previous value.
    }
  }, [client, refreshSupervisor]);

  useEffect(() => {
    void load();
  }, [load]);

  // One interval ticks the shared clock for every row's elapsed display.
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), CLOCK_TICK_MS);
    return () => clearInterval(timer);
  }, []);

  useEffect(
    () => () => {
      if (supervisorRefreshTimer.current) clearTimeout(supervisorRefreshTimer.current);
    },
    [],
  );

  // Live-update from the SSE hub. book.state/stage.progress patch existing rows;
  // queue.stats feeds the header strip. Every durable book-scoped frame also bumps
  // the book's event counter so an open log panel refetches (eta.update/queue.stats
  // do not - they write no per-book log line). A book newly created elsewhere is
  // picked up on the next full load (e.g. after switching back to this tab).
  const onEvent = useCallback(
    (type: PipelineEventType, data: unknown) => {
      if (type === 'book.state') {
        const ev = data as BookStateEvent;
        setBooks((prev) => (prev ? sortBooks(applyBookState(prev, ev)) : prev));
        setBookEventCounts((prev) => bumpBookEventCount(prev, ev.book_id));
      } else if (type === 'stage.progress') {
        const ev = data as StageProgressEvent;
        setBooks((prev) => (prev ? applyStageProgress(prev, ev) : prev));
        setBookEventCounts((prev) => bumpBookEventCount(prev, ev.book_id));
      } else if (type === 'stage.note') {
        const ev = data as StageNoteEvent;
        setBookEventCounts((prev) => bumpBookEventCount(prev, ev.book_id));
      } else if (type === 'contrib.update') {
        const ev = data as ContributionUpdateEvent;
        setBookEventCounts((prev) => bumpBookEventCount(prev, ev.book_id));
      } else if (type === 'queue.stats') {
        setStats(data as QueueStatsEvent);
      } else if (type === 'eta.update') {
        const ev = data as EtaUpdateEvent;
        // Patch each listed book's ETA and clear it on the rest (they lost their
        // ETA - parked/terminal); keep the queue makespan for the strip.
        setBooks((prev) => (prev ? applyEtaUpdate(prev, ev) : prev));
        setQueueSeconds(ev.queue_seconds);
      } else if (type === 'supervisor.decision') {
        const run = data as SupervisorRun;
        setIncidents((previous) =>
          [run, ...previous.filter((item) => item.id !== run.id)].slice(0, 8),
        );
        if (supervisorRefreshTimer.current) clearTimeout(supervisorRefreshTimer.current);
        supervisorRefreshTimer.current = setTimeout(() => {
          supervisorRefreshTimer.current = null;
          void refreshSupervisor(run.batch_id).catch(() => undefined);
        }, SUPERVISOR_REFRESH_DEBOUNCE_MS);
      }
    },
    [refreshSupervisor],
  );

  useEventStream(apiBase, token, { onEvent });

  // Stable callback for a row to lazily fetch its detail (with the stage-run cost
  // ledger) when expanded.
  const getDetail = useCallback((id: number) => client.getBook(id), [client]);

  // Stable callback for a row to lazily fetch its event log when expanded.
  const getEvents = useCallback(
    (id: number, limit?: number, beforeId?: number) => client.getBookEvents(id, limit, beforeId),
    [client],
  );

  // Stable across renders so the memoized BookRow only re-renders on its own props.
  const handleCompleteCoreProposal = useCallback((book: BookView) => setCoreBook(book), []);
  const handleAskSupervisor = useCallback(
    async (book: BookView) => {
      setActionError(null);
      try {
        await client.askSupervisor(book.id);
        await load();
      } catch (err) {
        setActionError(err instanceof ApiError ? err.message : 'Supervisor request failed.');
      }
    },
    [client, load],
  );

  // Stable across renders (deps: client, load) so the memoized BookRow only
  // re-renders when its own props change, not on every parent render.
  const handleAction = useCallback(
    async (id: number, action: BookAction) => {
      if (
        action === 'cancel' &&
        !window.confirm('Cancel this book? Its progress will be stopped.')
      ) {
        return;
      }
      if (
        action === 'purge' &&
        !window.confirm(
          'Free the split audio for this book? The chapter FLACs are deleted; ' +
            'they will be re-created from the source if the book runs again.',
        )
      ) {
        return;
      }
      setBusyId(id);
      setActionError(null);
      try {
        switch (action) {
          case 'pause':
            await client.pauseBook(id);
            break;
          case 'resume':
            await client.resumeBook(id);
            break;
          case 'retry':
            await client.retryBook(id);
            break;
          case 'cancel':
            await client.cancelBook(id);
            break;
          case 'delete':
            await client.deleteBook(id);
            break;
          case 'purge':
            await client.purgeScratch(id);
            break;
        }
        await load();
      } catch (err) {
        setActionError(err instanceof ApiError ? err.message : 'The action failed.');
      } finally {
        setBusyId(null);
      }
    },
    [client, load],
  );

  if (loadError) {
    return (
      <div className="rounded-xl border border-edge bg-surface p-6">
        <p role="alert" className="text-sm text-pink-500">
          {loadError}
        </p>
      </div>
    );
  }

  if (!books) {
    return (
      <div className="rounded-xl border border-edge bg-surface p-6">
        <p className="text-sm text-dim">Loading pipeline...</p>
      </div>
    );
  }

  const active = books.filter((b) => !isDone(b));
  const done = books.filter(isDone);

  return (
    <div className="flex flex-col gap-4">
      <QueueStrip
        stats={stats}
        queueSeconds={queueSeconds}
        activeCount={active.length}
        doneCount={done.length}
        scratchBytes={scratchBytes}
      />

      {supervisorStatus && (
        <SupervisorStrip status={supervisorStatus} incidents={incidents} costs={batchCosts} />
      )}

      {actionError && (
        <p role="alert" className="text-sm text-pink-500">
          {actionError}
        </p>
      )}

      {books.length === 0 ? (
        <div className="rounded-xl border border-edge bg-surface p-8 text-sm text-dim">
          Nothing in the pipeline yet. Scan a folder in the Library tab and process some books.
        </div>
      ) : (
        <div className="flex flex-col gap-2">
          {books.map((b) => (
            <BookRow
              key={b.id}
              book={b}
              busy={busyId === b.id}
              // Only rows that can render an elapsed readout (active, with a start
              // time) receive the ticking clock; the rest get a stable 0 so the
              // 30s tick does not break their memo (notably done rows, which render
              // no elapsed).
              now={!isDone(b) && b.started_at ? now : 0}
              // Changes on each durable book-scoped SSE frame, triggering the open
              // log panel's throttled refetch.
              eventCount={bookEventCounts[b.id] ?? 0}
              onAction={handleAction}
              getDetail={getDetail}
              getEvents={getEvents}
              onCompleteCoreProposal={handleCompleteCoreProposal}
              onAskSupervisor={handleAskSupervisor}
              modelSupervisorEnabled={
                !!(supervisorStatus?.model_assisted && supervisorStatus.model_available)
              }
            />
          ))}
        </div>
      )}

      {coreBook && (
        <CoreProposalModal
          book={coreBook}
          client={client}
          onClose={() => setCoreBook(null)}
          onDone={() => {
            setCoreBook(null);
            void load();
          }}
        />
      )}
    </div>
  );
}

function SupervisorStrip({
  status,
  incidents,
  costs,
}: {
  status: SupervisorStatus;
  incidents: SupervisorRun[];
  costs: BatchCostSummary | null;
}) {
  return (
    <div className="rounded-xl border border-edge bg-surface px-4 py-3 text-xs text-dim">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
        <span className="font-semibold text-hi">Supervisor: {status.state}</span>
        <span>automatic {status.automatic_actions ? 'on' : 'off'}</span>
        <span>
          model {status.model_assisted ? (status.model_available ? 'on' : 'unavailable') : 'off'}
        </span>
        {costs && (
          <span>
            production {formatCost(costs.production_reported_usd)} reported
            {costs.production_reported_incomplete ? ' (partial)' : ''}
          </span>
        )}
        {costs && (
          <span>
            production {formatCost(costs.production_estimated_api_usd)} API-equivalent estimate
            {costs.production_estimate_incomplete ? ' (partial)' : ''}
          </span>
        )}
        {costs && (
          <span>
            book supervision {formatCost(costs.book_supervisor_reported_usd)} reported /{' '}
            {formatCost(costs.book_supervisor_estimated_api_usd)} API-equivalent
          </span>
        )}
        {costs && (
          <span>
            batch supervision {formatCost(costs.batch_supervisor_reported_usd)} reported /{' '}
            {formatCost(costs.batch_supervisor_estimated_api_usd)} API-equivalent
          </span>
        )}
        {costs && (
          <span>
            overall {formatCost(costs.overall_reported_usd)} reported
            {costs.overall_reported_incomplete ? ' (partial)' : ''} /{' '}
            {formatCost(costs.overall_estimated_api_usd)} API-equivalent
            {costs.overall_estimate_incomplete ? ' (partial)' : ''}
          </span>
        )}
      </div>
      {incidents.length > 0 && (
        <div className="mt-2 border-t border-edge/50 pt-2">
          <span className="font-medium text-body">Recent: </span>
          {incidents
            .slice(0, 3)
            .map((incident) => `${incident.diagnosis} → ${incident.selected_action}`)
            .join(' · ')}
        </div>
      )}
    </div>
  );
}

interface QueueStripProps {
  stats: QueueStatsEvent | null;
  // Estimated makespan for the whole queue (from eta.update); null when it cannot
  // be estimated or none has arrived yet.
  queueSeconds: number | null;
  activeCount: number;
  doneCount: number;
  // Daemon-total on-disk scratch (from /system); null until first fetched.
  scratchBytes: number | null;
}

function QueueStrip({
  stats,
  queueSeconds,
  activeCount,
  doneCount,
  scratchBytes,
}: QueueStripProps) {
  return (
    <div className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-xl border border-edge bg-surface px-4 py-3 text-sm">
      <Stat label="Active" value={activeCount} />
      <Stat label="Done" value={doneCount} />
      {stats && (
        <>
          <span className="hidden text-edge sm:inline">|</span>
          <Stat label="ASR" value={stats.asr_active} muted />
          <Stat label="Agent" value={stats.agent_active} muted />
          <Stat label="Mechanical" value={stats.mechanical_active} muted />
          <Stat label="Queued" value={stats.queued} muted />
        </>
      )}
      {queueSeconds !== null && (
        <>
          <span className="hidden text-edge sm:inline">|</span>
          <span className="flex items-baseline gap-1.5">
            <span className="font-semibold text-hi">{formatEta(queueSeconds)}</span>
            <span className="text-xs uppercase tracking-wide text-dim">Queue ETA</span>
          </span>
        </>
      )}
      {scratchBytes !== null && (
        <>
          <span className="hidden text-edge sm:inline">|</span>
          <span className="flex items-baseline gap-1.5">
            <span className="font-semibold text-hi">{formatBytes(scratchBytes)}</span>
            <span className="text-xs uppercase tracking-wide text-dim">Scratch</span>
          </span>
        </>
      )}
    </div>
  );
}

function Stat({ label, value, muted }: { label: string; value: number; muted?: boolean }) {
  return (
    <span className="flex items-baseline gap-1.5">
      <span className={muted ? 'text-dim' : 'font-semibold text-hi'}>{value}</span>
      <span className="text-xs uppercase tracking-wide text-dim">{label}</span>
    </span>
  );
}
