import { useEffect, useRef, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type { MetaSearchResult, ScannedBook } from '@/api/types';

interface MatchModalProps {
  client: ApiClient;
  book: ScannedBook;
  onClose: () => void;
  // Called with the chosen work id; the parent applies the override.
  onPick: (result: MetaSearchResult) => void;
}

const DEBOUNCE_MS = 300;

// MatchModal lets the user manually match a book to a known work when auto-detect
// failed. It debounces a title search against meta.audiosilo.app, lists results,
// and hands the chosen work back to the parent. The metadata service being off
// (503) surfaces a friendly message rather than a raw error.
export function MatchModal({ client, book, onClose, onPick }: MatchModalProps) {
  const [query, setQuery] = useState(book.title ?? '');
  const [results, setResults] = useState<MetaSearchResult[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Guards against an out-of-order response overwriting a newer query's results.
  const seqRef = useRef(0);

  useEffect(() => {
    const trimmed = query.trim();
    if (trimmed === '') {
      setResults([]);
      setError(null);
      setLoading(false);
      return;
    }
    const seq = ++seqRef.current;
    setLoading(true);
    setError(null);
    const timer = setTimeout(() => {
      client
        .metaSearch(trimmed)
        .then((res) => {
          if (seq !== seqRef.current) return;
          setResults(res.results);
          setLoading(false);
        })
        .catch((err: unknown) => {
          if (seq !== seqRef.current) return;
          setResults([]);
          setLoading(false);
          if (err instanceof ApiError && err.status === 503) {
            setError(
              'Metadata lookup is disabled on this daemon, so manual matching is unavailable.',
            );
          } else if (err instanceof ApiError) {
            setError(err.message);
          } else {
            setError('Could not reach the metadata service.');
          }
        });
    }, DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [query, client]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/60 p-4 sm:p-8"
      role="dialog"
      aria-modal="true"
      aria-label="Match a book"
      onClick={onClose}
    >
      <div
        className="mt-8 w-full max-w-lg rounded-xl border border-edge bg-surface p-5 shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="mb-3 flex items-start justify-between gap-3">
          <div>
            <h3 className="text-base font-medium text-hi">Match a book</h3>
            <p className="mt-0.5 text-xs text-dim">
              Search meta.audiosilo.app for the work this recording belongs to.
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="rounded p-1 text-dim transition-colors hover:text-hi"
          >
            &#10005;
          </button>
        </div>

        <input
          type="text"
          value={query}
          autoFocus
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search by title..."
          aria-label="Search works"
          className="w-full rounded-md border border-edge bg-raised px-3 py-2 text-sm text-body placeholder:text-dim"
        />

        <div className="mt-3 max-h-80 overflow-y-auto">
          {error && (
            <p role="alert" className="px-1 py-2 text-sm text-pink-500">
              {error}
            </p>
          )}
          {!error && loading && <p className="px-1 py-2 text-sm text-dim">Searching...</p>}
          {!error && !loading && query.trim() !== '' && results.length === 0 && (
            <p className="px-1 py-2 text-sm text-dim">No matching works found.</p>
          )}
          <ul className="flex flex-col gap-1">
            {results.map((r) => (
              <li key={r.id}>
                <button
                  type="button"
                  onClick={() => onPick(r)}
                  className="w-full rounded-md border border-transparent px-2 py-2 text-left transition-colors hover:border-edge hover:bg-raised"
                >
                  <div className="text-sm font-medium text-hi">{r.title}</div>
                  {r.authors.length > 0 && (
                    <div className="text-xs text-body">{r.authors.join(', ')}</div>
                  )}
                  {r.series && (
                    <div className="text-xs text-dim">
                      {r.series.name}
                      {r.series.position ? ` #${r.series.position}` : ''}
                    </div>
                  )}
                </button>
              </li>
            ))}
          </ul>
        </div>
      </div>
    </div>
  );
}
