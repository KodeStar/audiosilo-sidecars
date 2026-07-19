import { memo } from 'react';
import type { ScannedBook } from '@/api/types';
import { isManualMatch, matchProvenanceLabel } from '@/lib/candidates';
import { stateLabel } from '@/lib/pipelineState';
import { CoverageBadge } from './CoverageBadge';

interface CandidateRowProps {
  book: ScannedBook;
  checked: boolean;
  onToggle: (path: string, checked: boolean) => void;
  // Row actions. A hidden row shows Unhide; a visible row shows Hide + Match (and
  // Clear match when the book carries a manual match). Omitted callbacks hide the
  // corresponding control.
  onMatch?: (book: ScannedBook) => void;
  onClearMatch?: (book: ScannedBook) => void;
  onHide?: (book: ScannedBook) => void;
  onUnhide?: (book: ScannedBook) => void;
  // Disables this row's actions while one of its overrides is in flight.
  busy?: boolean;
}

// A provenance-labelled identity chip (ASIN/ISBN). The title tooltip surfaces
// where the value came from (tag / path / filename) via the scan's sources map.
function IdentityChip({
  kind,
  value,
  source,
}: {
  kind: 'ASIN' | 'ISBN';
  value: string;
  source: string | undefined;
}) {
  const provenance = source ? ` (from ${source})` : '';
  return (
    <span
      title={`${kind} ${value}${provenance}`}
      className="inline-flex items-center gap-1 rounded border border-edge bg-raised px-1.5 py-0.5 font-mono text-[10px] text-dim"
    >
      <span className="text-[9px] font-semibold uppercase tracking-wide text-body">{kind}</span>
      {value}
    </span>
  );
}

function RowButton({
  onClick,
  disabled,
  children,
  title,
}: {
  onClick: () => void;
  disabled?: boolean;
  children: React.ReactNode;
  title?: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      title={title}
      className="rounded border border-edge bg-raised px-2 py-1 text-xs text-body transition-colors hover:bg-edge disabled:cursor-not-allowed disabled:opacity-50"
    >
      {children}
    </button>
  );
}

// CandidateRow is memoized. To be clear about what that does and does NOT buy:
// while a scan is running, every ~700ms poll parses a fresh JSON payload, so each
// book object is a new reference and the memo does NOT skip (identities change
// every tick). The memo pays off once polling stops - then a re-render driven by a
// selection toggle, a busy-state change, or a hide/match action only reconciles the
// rows whose own props actually changed, instead of every row. It relies on the
// parent passing stable callbacks (onToggle et al. are bound once) so an unchanged
// row's props stay referentially equal.
export const CandidateRow = memo(function CandidateRow({
  book,
  checked,
  onToggle,
  onMatch,
  onClearMatch,
  onHide,
  onUnhide,
  busy = false,
}: CandidateRowProps) {
  const authors = (book.authors ?? []).join(', ');
  const seriesText =
    book.series && book.series_position
      ? `${book.series} #${book.series_position}`
      : (book.series ?? '');

  const runtimeText =
    book.runtime_min && book.runtime_min > 0 ? formatRuntime(book.runtime_min) : '';
  const chapterText = book.chapters && book.chapters > 0 ? `${book.chapters} ch` : '';

  const hidden = !!book.hidden;
  const pipelineBook = book.pipeline_book;
  const provenance = matchProvenanceLabel(book.coverage);
  const manual = isManualMatch(book.coverage);
  const pipeline = pipelineBook ? pipelinePresence(pipelineBook.state, pipelineBook.status) : null;

  return (
    <tr
      className={
        'border-t border-edge align-top hover:bg-raised/40' + (hidden ? ' opacity-60' : '')
      }
    >
      <td className="px-3 py-3">
        {pipelineBook ? (
          <span className="text-dim" aria-hidden="true">
            -
          </span>
        ) : (
          <input
            type="checkbox"
            checked={checked}
            disabled={hidden}
            onChange={(e) => onToggle(book.path, e.target.checked)}
            aria-label={`Select ${book.title}`}
            className="mt-0.5 h-4 w-4 accent-pink-600 disabled:cursor-not-allowed disabled:opacity-40"
          />
        )}
      </td>
      <td className="px-3 py-3">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-medium text-hi">{book.title}</span>
          {pipeline && pipelineBook && (
            <span
              className={`inline-flex rounded border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${pipeline.className}`}
              title={`Pipeline book #${pipelineBook.id}: ${stateLabel(pipelineBook.state)}`}
            >
              {pipeline.label}
            </span>
          )}
        </div>
        {book.subtitle && <div className="text-xs text-dim">{book.subtitle}</div>}
        {authors && <div className="text-xs text-body">{authors}</div>}
        <div className="mt-1 flex flex-wrap items-center gap-1.5">
          {book.asin && <IdentityChip kind="ASIN" value={book.asin} source={book.sources?.asin} />}
          {book.isbn && <IdentityChip kind="ISBN" value={book.isbn} source={book.sources?.isbn} />}
        </div>
      </td>
      <td className="px-3 py-3 text-sm text-body">
        {seriesText || <span className="text-dim">-</span>}
      </td>
      <td className="px-3 py-3 text-sm text-body">
        {runtimeText || chapterText ? (
          <div className="flex flex-col">
            {runtimeText && <span>{runtimeText}</span>}
            {chapterText && <span className="text-xs text-dim">{chapterText}</span>}
          </div>
        ) : (
          <span className="text-dim">-</span>
        )}
      </td>
      <td className="px-3 py-3">
        <CoverageBadge coverage={book.coverage} />
        {provenance && (
          <div className="mt-1 text-[11px] italic text-dim" title={provenance}>
            {provenance}
          </div>
        )}
      </td>
      <td className="px-3 py-3">
        <div className="flex flex-wrap justify-end gap-1.5">
          {hidden ? (
            onUnhide && (
              <RowButton
                onClick={() => onUnhide(book)}
                disabled={busy}
                title="Show this book again"
              >
                Unhide
              </RowButton>
            )
          ) : (
            <>
              {!pipelineBook && onMatch && (
                <RowButton
                  onClick={() => onMatch(book)}
                  disabled={busy}
                  title="Match this book against meta.audiosilo.app"
                >
                  Match
                </RowButton>
              )}
              {!pipelineBook && manual && onClearMatch && (
                <RowButton
                  onClick={() => onClearMatch(book)}
                  disabled={busy}
                  title="Remove the manual match"
                >
                  Clear match
                </RowButton>
              )}
              {onHide && (
                <RowButton
                  onClick={() => onHide(book)}
                  disabled={busy}
                  title="Hide this book from the list"
                >
                  Hide
                </RowButton>
              )}
            </>
          )}
        </div>
      </td>
    </tr>
  );
});

function pipelinePresence(state: string, status: string): { label: string; className: string } {
  if (state === 'done') {
    return { label: 'Completed', className: 'border-success/40 bg-success/10 text-success' };
  }
  switch (status) {
    case 'paused':
      return {
        label: 'Paused',
        className: 'border-amber-500/40 bg-amber-500/10 text-amber-300',
      };
    case 'needs_attention':
      return {
        label: 'Needs attention',
        className: 'border-orange-500/40 bg-orange-500/10 text-orange-300',
      };
    case 'failed':
      return {
        label: 'Failed',
        className: 'border-pink-500/40 bg-pink-500/10 text-pink-300',
      };
    default:
      return {
        label: 'In queue',
        className: 'border-sky-500/40 bg-sky-500/10 text-sky-300',
      };
  }
}

function formatRuntime(minutes: number): string {
  const h = Math.floor(minutes / 60);
  const m = minutes % 60;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}
