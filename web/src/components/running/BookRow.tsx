import type { BookView } from '@/api/types';
import { availableActions, isDone, type BookAction } from '@/lib/books';
import { laneOf, stateChipClass, stateLabel, statusBadge } from '@/lib/pipelineState';

interface BookRowProps {
  book: BookView;
  busy: boolean;
  onAction: (id: number, action: BookAction) => void;
}

const ACTION_LABEL: Record<BookAction, string> = {
  pause: 'Pause',
  resume: 'Resume',
  retry: 'Retry',
  cancel: 'Cancel',
  delete: 'Delete',
};

export function BookRow({ book, busy, onAction }: BookRowProps) {
  const done = isDone(book);
  const badge = statusBadge(book.status);
  const seriesText =
    book.series && book.series_pos ? `${book.series} #${book.series_pos}` : book.series;
  const stageProgress = activeProgress(book);

  return (
    <div
      className={
        'flex flex-col gap-3 rounded-xl border border-edge p-4 sm:flex-row sm:items-center sm:justify-between ' +
        (done ? 'bg-surface/50 opacity-70' : 'bg-surface')
      }
    >
      <div className="min-w-0 flex-1">
        <div className="truncate font-medium text-hi">{book.title}</div>
        {seriesText && <div className="text-xs text-dim">{seriesText}</div>}
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <span
            className={
              'inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-medium ' +
              stateChipClass(book.state)
            }
            title={`Lane: ${laneOf(book.state)}`}
          >
            {stateLabel(book.state)}
          </span>
          {badge && (
            <span
              className={
                'inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium ' +
                badge.className
              }
            >
              {badge.label}
            </span>
          )}
          {stageProgress && (
            <span className="text-[11px] text-dim">
              {stageProgress.done}/{stageProgress.total}
            </span>
          )}
        </div>
        {book.error && book.status !== 'paused' && (
          <p className="mt-1 text-xs text-pink-500">{book.error}</p>
        )}
      </div>

      <div className="flex shrink-0 flex-wrap gap-2">
        {availableActions(book).map((action) => (
          <button
            key={action}
            type="button"
            disabled={busy}
            onClick={() => onAction(book.id, action)}
            className={
              'rounded-md border px-3 py-1.5 text-xs font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50 ' +
              (action === 'cancel' || action === 'delete'
                ? 'border-edge text-dim hover:border-pink-600 hover:text-pink-400'
                : 'border-edge text-body hover:border-pink-600 hover:text-hi')
            }
          >
            {ACTION_LABEL[action]}
          </button>
        ))}
      </div>
    </div>
  );
}

// activeProgress returns the counter for the book's current stage, if any.
function activeProgress(book: BookView): { done: number; total: number } | null {
  const p = book.progress?.find((x) => x.stage === book.state);
  if (!p || p.total <= 0) return null;
  return { done: p.done, total: p.total };
}
