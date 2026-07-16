import { useCallback, useEffect, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type { BookStateEvent, BookView, QueueStatsEvent, StageProgressEvent } from '@/api/types';
import {
  applyBookState,
  applyStageProgress,
  formatBytes,
  isDone,
  sortBooks,
  type BookAction,
} from '@/lib/books';
import { useEventStream, type PipelineEventType } from '@/lib/useEventStream';
import { BookRow } from '../running/BookRow';

interface RunningPanelProps {
  client: ApiClient;
  apiBase: string;
  token: string;
}

export function RunningPanel({ client, apiBase, token }: RunningPanelProps) {
  const [books, setBooks] = useState<BookView[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [stats, setStats] = useState<QueueStatsEvent | null>(null);
  const [scratchBytes, setScratchBytes] = useState<number | null>(null);
  const [busyId, setBusyId] = useState<number | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

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
    }
  }, []);

  useEventStream(apiBase, token, { onEvent });

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
            <BookRow key={b.id} book={b} busy={busyId === b.id} onAction={handleAction} />
          ))}
        </div>
      )}
    </div>
  );
}

interface QueueStripProps {
  stats: QueueStatsEvent | null;
  activeCount: number;
  doneCount: number;
  // Daemon-total on-disk scratch (from /system); null until first fetched.
  scratchBytes: number | null;
}

function QueueStrip({ stats, activeCount, doneCount, scratchBytes }: QueueStripProps) {
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
