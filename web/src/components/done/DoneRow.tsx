import { memo, useCallback, useState } from 'react';
import type { BookDetail, BookView, SidecarsView } from '@/api/types';
import { formatBytes } from '@/lib/books';
import { formatCost } from '@/lib/cost';
import { formatFinishedDate } from '@/lib/doneBoard';
import { useLazyDetail } from '@/lib/useLazyDetail';
import { StageCostTable } from './StageCostTable';
import { SidecarsPreview } from './SidecarsPreview';

interface DoneRowProps {
  book: BookView;
  busy: boolean;
  // Purge frees the on-disk scratch; delete removes the book. Both confirmed by the
  // panel, which reloads afterward.
  onPurge: (id: number) => void;
  onDelete: (id: number) => void;
  // Lazily fetches the book's detail (with the stage-run ledger) when Details opens.
  getDetail: (id: number) => Promise<BookDetail>;
  // Lazily fetches the extracted characters/recaps for the Preview modal.
  getSidecars: (id: number) => Promise<SidecarsView>;
}

// DoneRow is one finished book on the Done board: identity, finished date, total
// cost, scratch (with a Purge control when any remains), a contribution-status chip
// (placeholder until M7), and Delete. It expands to a per-stage cost table and opens
// a Preview modal with the sidecars. Memoized like BookRow.
export const DoneRow = memo(function DoneRow({
  book,
  busy,
  onPurge,
  onDelete,
  getDetail,
  getSidecars,
}: DoneRowProps) {
  const seriesText =
    book.series && book.series_pos ? `${book.series} #${book.series_pos}` : book.series;
  const authors = book.authors.length > 0 ? book.authors.join(', ') : null;

  const { expanded, toggle, detail, detailState } = useLazyDetail<BookDetail>(getDetail, book.id);
  const [preview, setPreview] = useState(false);

  // Stable across re-renders so the mounted SidecarsPreview's fetch effect (keyed
  // on this callback) doesn't re-run when the row re-renders for another reason.
  const loadSidecars = useCallback(() => getSidecars(book.id), [getSidecars, book.id]);

  return (
    <div className="flex flex-col gap-3 rounded-xl border border-edge bg-surface p-4">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="min-w-0 flex-1">
          <div className="truncate font-medium text-hi">{book.title}</div>
          {authors && <div className="truncate text-xs text-dim">{authors}</div>}
          {seriesText && <div className="text-xs text-dim">{seriesText}</div>}
          <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-dim">
            <span title="When the book finished">
              Finished {formatFinishedDate(book.updated_at)}
            </span>
            <span className="text-edge">|</span>
            <span title="Total agent spend for this book">{formatCost(book.total_cost_usd)}</span>
            {book.scratch_bytes > 0 && (
              <>
                <span className="text-edge">|</span>
                <span title="Scratch on disk (chapters + durables)">
                  {formatBytes(book.scratch_bytes)} on disk
                </span>
              </>
            )}
            <span
              className="rounded-full border border-edge bg-raised px-2 py-0.5 text-[0.65rem] uppercase tracking-wide text-dim"
              title="Contribution (intake issue / PR) lands in M7"
            >
              Local only
            </span>
          </div>
        </div>

        <div className="flex shrink-0 flex-wrap gap-2">
          <button
            type="button"
            onClick={() => setPreview(true)}
            className="rounded-md border border-edge px-3 py-1.5 text-xs font-medium text-body transition-colors hover:border-pink-600 hover:text-hi"
          >
            Preview
          </button>
          <button
            type="button"
            aria-expanded={expanded}
            onClick={toggle}
            className="rounded-md border border-edge px-3 py-1.5 text-xs font-medium text-body transition-colors hover:border-pink-600 hover:text-hi"
          >
            {expanded ? 'Hide details' : 'Details'}
          </button>
          {book.scratch_bytes > 0 && (
            <button
              type="button"
              disabled={busy}
              onClick={() => onPurge(book.id)}
              className="rounded-md border border-edge px-3 py-1.5 text-xs font-medium text-dim transition-colors hover:border-pink-600 hover:text-pink-400 disabled:cursor-not-allowed disabled:opacity-50"
            >
              Free disk
            </button>
          )}
          <button
            type="button"
            disabled={busy}
            onClick={() => onDelete(book.id)}
            className="rounded-md border border-edge px-3 py-1.5 text-xs font-medium text-dim transition-colors hover:border-pink-600 hover:text-pink-400 disabled:cursor-not-allowed disabled:opacity-50"
          >
            Delete
          </button>
        </div>
      </div>

      {expanded && (
        <div className="rounded-lg border border-edge/50 bg-raised/40 p-3">
          {detailState === 'loading' && <p className="text-xs text-dim">Loading details...</p>}
          {detailState === 'error' && (
            <p className="text-xs text-pink-500">Could not load stage details.</p>
          )}
          {detailState === 'idle' && detail && <StageCostTable runs={detail.stage_runs} />}
        </div>
      )}

      {preview && (
        <SidecarsPreview
          title={book.title}
          getSidecars={loadSidecars}
          onClose={() => setPreview(false)}
        />
      )}
    </div>
  );
});
