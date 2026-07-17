import { useCallback, useEffect, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type { BookView } from '@/api/types';
import { filterDoneBooks } from '@/lib/doneBoard';
import { useThrottledCallback } from '@/lib/throttleTrailing';
import { useEventStream, type PipelineEventType } from '@/lib/useEventStream';
import { DoneRow } from '../done/DoneRow';

interface DonePanelProps {
  client: ApiClient;
  apiBase: string;
  token: string;
}

// Coalesce a burst of contrib.update frames (a book contributes several dimensions
// at once) into one refetch.
const REFETCH_THROTTLE_MS = 1500;

// DonePanel lists finished books (state === 'done'), newest first. It reloads on
// mount, after each row action, and - via the SSE hub - on every contrib.update
// frame (throttled) so the contribution chips stay live while the tab is open. A
// book that newly *finishes* while the tab is open is still only picked up on a
// remount (its state transition is a book.state frame the Running tab owns); the
// contrib.update subscription keeps the already-listed done books' chips current.
export function DonePanel({ client, apiBase, token }: DonePanelProps) {
  const [books, setBooks] = useState<BookView[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<number | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const { books: list } = await client.listBooks();
      setBooks(filterDoneBooks(list));
      setLoadError(null);
    } catch (err) {
      setLoadError(err instanceof ApiError ? err.message : 'Could not load finished books.');
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  // Throttle the contrib.update-driven refetch: at most one reload per window.
  const scheduleRefetch = useThrottledCallback(
    useCallback(() => void load(), [load]),
    REFETCH_THROTTLE_MS,
  );

  const onEvent = useCallback(
    (type: PipelineEventType) => {
      if (type === 'contrib.update') scheduleRefetch();
    },
    [scheduleRefetch],
  );

  useEventStream(apiBase, token, { onEvent });

  const getDetail = useCallback((id: number) => client.getBook(id), [client]);
  const getSidecars = useCallback((id: number) => client.getBookSidecars(id), [client]);
  const getExport = useCallback((id: number) => client.exportSidecars(id), [client]);

  // runAction is the shared confirm -> busy -> clear-error -> call -> reload ->
  // catch -> finally lifecycle every row action follows. The caller supplies the
  // confirm prompt and the API call so the two handlers stay one-liners.
  const runAction = useCallback(
    async (id: number, confirmMessage: string, call: () => Promise<void>) => {
      if (!window.confirm(confirmMessage)) return;
      setBusyId(id);
      setActionError(null);
      try {
        await call();
        await load();
      } catch (err) {
        setActionError(err instanceof ApiError ? err.message : 'The action failed.');
      } finally {
        setBusyId(null);
      }
    },
    [load],
  );

  const handlePurge = useCallback(
    (id: number) =>
      runAction(
        id,
        'Free the split audio for this book? The chapter FLACs are deleted; the ' +
          'finished sidecars are kept.',
        () => client.purgeScratch(id),
      ),
    [runAction, client],
  );

  const handleDelete = useCallback(
    (id: number) =>
      runAction(id, 'Delete this book and its work directory? This cannot be undone.', () =>
        client.deleteBook(id),
      ),
    [runAction, client],
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
        <p className="text-sm text-dim">Loading finished books...</p>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      {actionError && (
        <p role="alert" className="text-sm text-pink-500">
          {actionError}
        </p>
      )}

      {books.length === 0 ? (
        <div className="rounded-xl border border-edge bg-surface p-8 text-sm text-dim">
          No finished books yet. Books that complete the pipeline show here with their sidecars
          preview and cost breakdown.
        </div>
      ) : (
        <div className="flex flex-col gap-2">
          {books.map((b) => (
            <DoneRow
              key={b.id}
              book={b}
              busy={busyId === b.id}
              onPurge={handlePurge}
              onDelete={handleDelete}
              getDetail={getDetail}
              getSidecars={getSidecars}
              getExport={getExport}
            />
          ))}
        </div>
      )}
    </div>
  );
}
