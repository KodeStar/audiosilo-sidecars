import { useCallback, useEffect, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type {
  BookStateEvent,
  BookView,
  EtaUpdateEvent,
  QueueStatsEvent,
  StageProgressEvent,
} from '@/api/types';
import {
  applyBookState,
  applyEtaUpdate,
  applyStageProgress,
  formatBytes,
  isDone,
  sortBooks,
  type BookAction,
} from '@/lib/books';
import { formatEta } from '@/lib/duration';
import { useEventStream, type PipelineEventType } from '@/lib/useEventStream';
import { BookRow } from '../running/BookRow';

// The elapsed clock is coarse (rows show whole minutes/seconds), so a 30s tick is
// plenty and keeps the panel cheap.
const CLOCK_TICK_MS = 30_000;

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
  // A single shared clock driving every row's elapsed display (no per-row timer).
  const [now, setNow] = useState(() => Date.now());

  // Reload the books AND the daemon-total scratch gauge together. Fetching the
  // gauge here (not just on mount) keeps it fresh after an action - notably a
  // purge, which drops the total. The two calls are independent, so a gauge
  // failure never blocks the pipeline list.
  const load = useCallback(async () => {
    try {
      const { books: list } = await client.listBooks();
      setBooks(sortBooks(list));
      setLoadError(null);
    } catch (err) {
      setLoadError(err instanceof ApiError ? err.message : 'Could not load the pipeline.');
    }
    try {
      const info = await client.system();
      setScratchBytes(info.scratch_bytes);
    } catch {
      // A gauge read failure is non-fatal; leave the previous value.
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  // One interval ticks the shared clock for every row's elapsed display.
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), CLOCK_TICK_MS);
    return () => clearInterval(timer);
  }, []);

  // Live-update from the SSE hub. book.state/stage.progress patch existing rows;
  // queue.stats feeds the header strip. A book newly created elsewhere is picked
  // up on the next full load (e.g. after switching back to this tab).
  const onEvent = useCallback((type: PipelineEventType, data: unknown) => {
    if (type === 'book.state') {
      const ev = data as BookStateEvent;
      setBooks((prev) => (prev ? sortBooks(applyBookState(prev, ev)) : prev));
    } else if (type === 'stage.progress') {
      const ev = data as StageProgressEvent;
      setBooks((prev) => (prev ? applyStageProgress(prev, ev) : prev));
    } else if (type === 'queue.stats') {
      setStats(data as QueueStatsEvent);
    } else if (type === 'eta.update') {
      const ev = data as EtaUpdateEvent;
      // Patch each listed book's ETA and clear it on the rest (they lost their
      // ETA - parked/terminal); keep the queue makespan for the strip.
      setBooks((prev) => (prev ? applyEtaUpdate(prev, ev) : prev));
      setQueueSeconds(ev.queue_seconds);
    }
  }, []);

  useEventStream(apiBase, token, { onEvent });

  // Stable callback for a row to lazily fetch its detail (with the stage-run cost
  // ledger) when expanded.
  const getDetail = useCallback((id: number) => client.getBook(id), [client]);

  // Stable callback for a row to lazily fetch its event log when expanded.
  const getEvents = useCallback(
    (id: number, limit?: number) => client.getBookEvents(id, limit),
    [client],
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
              onAction={handleAction}
              getDetail={getDetail}
              getEvents={getEvents}
            />
          ))}
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
