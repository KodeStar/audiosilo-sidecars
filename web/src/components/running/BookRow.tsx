import { memo, useState } from 'react';
import type { BookDetail, BookView } from '@/api/types';
import { availableActions, formatBytes, isDone, type BookAction } from '@/lib/books';
import { formatCost } from '@/lib/cost';
import { normalizeLane, stateChipClass, stateLabel, statusBadge } from '@/lib/pipelineState';
import { StageCostList } from './StageCostList';

interface BookRowProps {
  book: BookView;
  busy: boolean;
  onAction: (id: number, action: BookAction) => void;
  // Lazily fetches the book's detail (with its stage-run cost ledger) when the row
  // is expanded. Kept as a prop so the row stays decoupled from ApiClient.
  getDetail: (id: number) => Promise<BookDetail>;
}

const ACTION_LABEL: Record<BookAction, string> = {
  pause: 'Pause',
  resume: 'Resume',
  retry: 'Retry',
  cancel: 'Cancel',
  delete: 'Delete',
  purge: 'Free disk',
};

// mildActions render in the neutral style; the rest (destructive/reclaim) in the
// warn style.
const MILD_ACTIONS = new Set<BookAction>(['pause', 'resume', 'retry']);

// BookRow is memoized: the Running list re-renders on every SSE patch, but a row
// whose props are unchanged (referential equality on the patched book object)
// should not re-render.
export const BookRow = memo(function BookRow({ book, busy, onAction, getDetail }: BookRowProps) {
  const done = isDone(book);
  const badge = statusBadge(book.status);
  const seriesText =
    book.series && book.series_pos ? `${book.series} #${book.series_pos}` : book.series;
  const stageProgress = activeProgress(book);

  const [expanded, setExpanded] = useState(false);
  const [detail, setDetail] = useState<BookDetail | null>(null);
  const [detailState, setDetailState] = useState<'idle' | 'loading' | 'error'>('idle');

  async function toggleDetails() {
    const next = !expanded;
    setExpanded(next);
    // Re-fetch each time it opens so the ledger reflects the latest runs.
    if (next) {
      setDetailState('loading');
      try {
        setDetail(await getDetail(book.id));
        setDetailState('idle');
      } catch {
        setDetailState('error');
      }
    }
  }

  return (
    <div
      className={
        'flex flex-col gap-3 rounded-xl border border-edge p-4 ' +
        (done ? 'bg-surface/50 opacity-70' : 'bg-surface')
      }
    >
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="min-w-0 flex-1">
          <div className="truncate font-medium text-hi">{book.title}</div>
          {seriesText && <div className="text-xs text-dim">{seriesText}</div>}
          <div className="mt-2 flex flex-wrap items-center gap-2">
            <span
              className={
                'inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-medium ' +
                stateChipClass(book.state, book.lane)
              }
              title={`Lane: ${normalizeLane(book.lane)}`}
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
            {book.scratch_bytes > 0 && (
              <span className="text-[11px] text-dim" title="Scratch on disk (chapters + durables)">
                {formatBytes(book.scratch_bytes)} on disk
              </span>
            )}
            {book.total_cost_usd > 0 && (
              <span className="text-[11px] text-dim" title="Total agent spend for this book">
                {formatCost(book.total_cost_usd)}
              </span>
            )}
          </div>
          {book.error && book.status !== 'paused' && (
            <p className="mt-1 text-xs text-pink-500">{book.error}</p>
          )}
        </div>

        <div className="flex shrink-0 flex-wrap gap-2">
          <button
            type="button"
            aria-expanded={expanded}
            onClick={() => void toggleDetails()}
            className="rounded-md border border-edge px-3 py-1.5 text-xs font-medium text-body transition-colors hover:border-pink-600 hover:text-hi"
          >
            {expanded ? 'Hide details' : 'Details'}
          </button>
          {availableActions(book).map((action) => (
            <button
              key={action}
              type="button"
              disabled={busy}
              onClick={() => onAction(book.id, action)}
              className={
                'rounded-md border px-3 py-1.5 text-xs font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50 ' +
                (MILD_ACTIONS.has(action)
                  ? 'border-edge text-body hover:border-pink-600 hover:text-hi'
                  : 'border-edge text-dim hover:border-pink-600 hover:text-pink-400')
              }
            >
              {ACTION_LABEL[action]}
            </button>
          ))}
        </div>
      </div>

      {expanded && (
        <div className="rounded-lg border border-edge/50 bg-raised/40 p-3">
          {detailState === 'loading' && <p className="text-xs text-dim">Loading details...</p>}
          {detailState === 'error' && (
            <p className="text-xs text-pink-500">Could not load stage details.</p>
          )}
          {detailState === 'idle' && detail && <StageCostList runs={detail.stage_runs} />}
        </div>
      )}
    </div>
  );
});

// activeProgress returns the counter for the book's current stage, if any.
function activeProgress(book: BookView): { done: number; total: number } | null {
  const p = book.progress?.find((x) => x.stage === book.state);
  if (!p || p.total <= 0) return null;
  return { done: p.done, total: p.total };
}
