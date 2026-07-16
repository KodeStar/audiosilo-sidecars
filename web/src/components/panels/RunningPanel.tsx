import { useCallback, useEffect, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type { BookStateEvent, BookView, QueueStatsEvent, StageProgressEvent } from '@/api/types';
import {
  applyBookState,
  applyStageProgress,
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
  const [busyId, setBusyId] = useState<number | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const { books: list } = await client.listBooks();
      setBooks(sortBooks(list));
      setLoadError(null);
    } catch (err) {
      setLoadError(err instanceof ApiError ? err.message : 'Could not load the pipeline.');
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

  async function handleAction(id: number, action: BookAction) {
    if (action === 'cancel' && !window.confirm('Cancel this book? Its progress will be stopped.')) {
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
      }
      await load();
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : 'The action failed.');
    } finally {
      setBusyId(null);
    }
  }

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
      <QueueStrip stats={stats} activeCount={active.length} doneCount={done.length} />

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
}

function QueueStrip({ stats, activeCount, doneCount }: QueueStripProps) {
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
