import type { ScannedBook } from '@/api/types';
import { CoverageBadge } from './CoverageBadge';

interface CandidateRowProps {
  book: ScannedBook;
  checked: boolean;
  onToggle: (path: string, checked: boolean) => void;
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

export function CandidateRow({ book, checked, onToggle }: CandidateRowProps) {
  const authors = (book.authors ?? []).join(', ');
  const seriesText =
    book.series && book.series_position
      ? `${book.series} #${book.series_position}`
      : (book.series ?? '');

  const runtimeText =
    book.runtime_min && book.runtime_min > 0 ? formatRuntime(book.runtime_min) : '';
  const chapterText = book.chapters && book.chapters > 0 ? `${book.chapters} ch` : '';

  return (
    <tr className="border-t border-edge align-top hover:bg-raised/40">
      <td className="px-3 py-3">
        <input
          type="checkbox"
          checked={checked}
          onChange={(e) => onToggle(book.path, e.target.checked)}
          aria-label={`Select ${book.title}`}
          className="mt-0.5 h-4 w-4 accent-pink-600"
        />
      </td>
      <td className="px-3 py-3">
        <div className="font-medium text-hi">{book.title}</div>
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
      </td>
    </tr>
  );
}

function formatRuntime(minutes: number): string {
  const h = Math.floor(minutes / 60);
  const m = minutes % 60;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}
