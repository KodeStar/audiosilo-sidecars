import { memo, useCallback, useState } from 'react';
import type { BookDetail, BookView, SidecarsView } from '@/api/types';
import { ApiError } from '@/lib/apiClient';
import { formatBytes } from '@/lib/books';
import { formatCost } from '@/lib/cost';
import {
  contributionChip,
  contributionRowLines,
  formatFinishedDate,
  type ContributionRowLine,
} from '@/lib/doneBoard';
import { triggerBlobDownload } from '@/lib/download';
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
  // Lazily fetches the book's detail (with the stage-run ledger + contribution rows)
  // when Details opens.
  getDetail: (id: number) => Promise<BookDetail>;
  // Lazily fetches the extracted characters/recaps for the Preview modal.
  getSidecars: (id: number) => Promise<SidecarsView>;
  // Fetches the sidecars export zip (blob + filename) for the Download action.
  getExport: (id: number) => Promise<{ blob: Blob; filename: string }>;
}

// The download control's transient state: idle -> downloading -> (idle | missing |
// error). `missing` (a 404) means the book has no sidecars to export, so the button
// disables itself with a note; `error` is a transient failure the user can retry.
type DownloadState = 'idle' | 'downloading' | 'missing' | 'error';

// DoneRow is one finished book on the Done board: identity, finished date, total
// cost, scratch (with a Purge control when any remains), a live contribution-status
// chip (issue/PR/merged/closed/local), a sidecars Download, and Delete. It expands to
// a per-stage cost table plus the per-kind contribution rows, and opens a Preview
// modal with the sidecars. Memoized like BookRow.
export const DoneRow = memo(function DoneRow({
  book,
  busy,
  onPurge,
  onDelete,
  getDetail,
  getSidecars,
  getExport,
}: DoneRowProps) {
  const seriesText =
    book.series && book.series_pos ? `${book.series} #${book.series_pos}` : book.series;
  const authors = book.authors.length > 0 ? book.authors.join(', ') : null;
  const chip = contributionChip(book.contribution);

  const { expanded, toggle, detail, detailState } = useLazyDetail<BookDetail>(getDetail, book.id);
  const [preview, setPreview] = useState(false);
  const [download, setDownload] = useState<DownloadState>('idle');

  // Stable across re-renders so the mounted SidecarsPreview's fetch effect (keyed
  // on this callback) doesn't re-run when the row re-renders for another reason.
  const loadSidecars = useCallback(() => getSidecars(book.id), [getSidecars, book.id]);

  const handleDownload = useCallback(async () => {
    setDownload('downloading');
    try {
      const { blob, filename } = await getExport(book.id);
      triggerBlobDownload(blob, filename);
      setDownload('idle');
    } catch (err) {
      // A 404 = no sidecar files for this book; disable the control. Anything else is
      // transient - leave the button enabled so the user can retry.
      setDownload(err instanceof ApiError && err.status === 404 ? 'missing' : 'error');
    }
  }, [getExport, book.id]);

  const contribRows = contributionRowLines(detail?.contributions);

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
            <span title="Provider-reported agent spend; detailed costs may also include API-equivalent estimates">
              {formatCost(book.total_cost_usd)} reported
            </span>
            {book.scratch_bytes > 0 && (
              <>
                <span className="text-edge">|</span>
                <span title="Scratch on disk (chapters + durables)">
                  {formatBytes(book.scratch_bytes)} on disk
                </span>
              </>
            )}
            <ContributionChipView chip={chip} />
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
            disabled={download === 'downloading' || download === 'missing'}
            onClick={() => void handleDownload()}
            title={
              download === 'missing'
                ? 'This book has no sidecar files to export'
                : 'Download the sidecars as a zip (repo layout)'
            }
            className="rounded-md border border-edge px-3 py-1.5 text-xs font-medium text-body transition-colors hover:border-pink-600 hover:text-hi disabled:cursor-not-allowed disabled:opacity-50"
          >
            {download === 'downloading'
              ? 'Preparing...'
              : download === 'missing'
                ? 'No sidecars'
                : 'Download'}
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

      {download === 'error' && (
        <p role="alert" className="text-xs text-pink-500">
          The download failed. Try again.
        </p>
      )}

      {expanded && (
        <div className="flex flex-col gap-3 rounded-lg border border-edge/50 bg-raised/40 p-3">
          {detailState === 'loading' && <p className="text-xs text-dim">Loading details...</p>}
          {detailState === 'error' && (
            <p className="text-xs text-pink-500">Could not load stage details.</p>
          )}
          {detailState === 'idle' && detail && (
            <>
              <StageCostTable runs={detail.stage_runs} />
              {contribRows.length > 0 && <ContributionRows rows={contribRows} />}
            </>
          )}
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

// ContributionChipView renders the aggregate contribution chip - a link when the
// summary carries a url, a plain pill otherwise. The `closed` (attention) status
// gets the warn tint.
function ContributionChipView({
  chip,
}: {
  chip: { label: string; url: string | null; attention: boolean };
}) {
  const className =
    'rounded-full border px-2 py-0.5 text-[0.65rem] uppercase tracking-wide ' +
    (chip.attention
      ? 'border-pink-500/40 bg-pink-500/10 text-pink-400'
      : 'border-edge bg-raised text-dim');
  if (chip.url) {
    return (
      <a
        href={chip.url}
        target="_blank"
        rel="noreferrer"
        title="Open the contribution on GitHub"
        className={className + ' transition-colors hover:text-hi'}
      >
        {chip.label}
      </a>
    );
  }
  return (
    <span className={className} title="Contribution status">
      {chip.label}
    </span>
  );
}

// ContributionRows lists the per-kind contribution rows in the expanded details:
// each dimension (characters/recaps/core), its status, a link when known, and any
// caveat note (e.g. the labels-missing hint).
function ContributionRows({ rows }: { rows: ContributionRowLine[] }) {
  return (
    <div className="flex flex-col gap-1.5">
      <div className="text-[11px] font-semibold uppercase tracking-wide text-dim">Contribution</div>
      <ul className="flex flex-col gap-1 text-xs">
        {rows.map((r) => (
          <li key={r.key} className="flex flex-col gap-0.5">
            <span className="flex flex-wrap items-baseline gap-x-2">
              {r.url ? (
                <a
                  href={r.url}
                  target="_blank"
                  rel="noreferrer"
                  className="font-medium text-body transition-colors hover:text-hi"
                >
                  {r.label}
                </a>
              ) : (
                <span className="font-medium text-body">{r.label}</span>
              )}
              <span className="text-dim">{r.statusLabel}</span>
            </span>
            {r.note && <span className="text-[11px] text-dim">{r.note}</span>}
          </li>
        ))}
      </ul>
    </div>
  );
}
