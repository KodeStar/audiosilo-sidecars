import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import type { MetaSearchResult, ScanJob, ScannedBook } from '@/api/types';
import {
  filterCandidates,
  hiddenBooks,
  searchCandidates,
  seriesGapHint,
  sortBySeries,
  toCandidate,
} from '@/lib/candidates';
import { scanStore, useScanStore } from '@/lib/scanStore';
import { runningScanDetail } from '@/lib/scanStatus';
import { addRecentRoot, loadRecentRoots } from '@/lib/recentRoots';
import { CandidateRow } from '../library/CandidateRow';
import { MatchModal } from '../library/MatchModal';

interface LibraryPanelProps {
  client: ApiClient;
  // Called after at least one book was enqueued, so the shell can switch to the
  // Running tab.
  onProcessed: () => void;
}

// A stable no-op for the hidden rows' onToggle (their checkbox is disabled), so
// the memoized CandidateRow sees a referentially-equal prop across renders.
const noopToggle = () => {};

export function LibraryPanel({ client, onProcessed }: LibraryPanelProps) {
  const state = useScanStore();
  const {
    job,
    scanError,
    starting,
    excludeCovered,
    showHidden,
    search,
    selected,
    processing,
    note,
  } = state;

  const [path, setPath] = useState('');
  const [recent, setRecent] = useState<string[]>(() => loadRecentRoots());
  const [matchTarget, setMatchTarget] = useState<ScannedBook | null>(null);
  const [busyPaths, setBusyPaths] = useState<Set<string>>(new Set());
  const [actionError, setActionError] = useState<string | null>(null);

  // Reattach to the last daemon scan on first mount (once per session - a tab
  // switch keeps the module-level store state without a network call).
  useEffect(() => {
    void scanStore.reattach(client);
  }, [client]);

  const startScan = useCallback(
    async (target: string) => {
      const trimmed = target.trim();
      if (trimmed === '') return;
      setActionError(null);
      const { ok } = await scanStore.startScan(client, trimmed);
      // Only remember the folder once the daemon accepted the scan - a rejected
      // path (e.g. outside library_roots) should not pollute the recent list.
      if (ok) setRecent(addRecentRoot(trimmed));
    },
    [client],
  );

  const books = useMemo<ScannedBook[]>(() => job?.books ?? [], [job]);
  // The visible list: exclude-covered filter -> free-text search -> series order.
  const visible = useMemo(
    () => sortBySeries(searchCandidates(filterCandidates(books, { excludeCovered }), search)),
    [books, excludeCovered, search],
  );
  // The full hidden set (drives the "Show hidden (n)" count + the toolbar totals),
  // and its search-narrowed, series-ordered slice for the dimmed hidden section.
  const hidden = useMemo(() => hiddenBooks(books), [books]);
  const hiddenVisible = useMemo(
    () => sortBySeries(searchCandidates(hidden, search)),
    [hidden, search],
  );
  const selectedVisible = useMemo(
    () => visible.filter((b) => !b.pipeline_book && selected.has(b.path)),
    [visible, selected],
  );
  const selectableVisible = useMemo(() => visible.filter((b) => !b.pipeline_book), [visible]);
  const satisfiedPaths = useMemo(() => {
    const paths = new Set(selectedVisible.map((b) => b.path));
    for (const b of visible) {
      if (b.pipeline_book) paths.add(b.path);
    }
    return paths;
  }, [visible, selectedVisible]);
  const gapSeries = useMemo(
    () => seriesGapHint(visible, satisfiedPaths),
    [visible, satisfiedPaths],
  );

  const allVisibleSelected =
    selectableVisible.length > 0 && selectableVisible.every((b) => selected.has(b.path));

  // Bound once so the memoized CandidateRow only reconciles rows that actually
  // changed across a streaming scan poll (a fresh inline closure would defeat it).
  const handleToggle = useCallback((p: string, on: boolean) => scanStore.toggleOne(p, on), []);

  const withBusy = useCallback(
    async (bookPath: string, run: () => Promise<{ ok: boolean; error?: string }>) => {
      setBusyPaths((prev) => new Set(prev).add(bookPath));
      setActionError(null);
      const res = await run();
      setBusyPaths((prev) => {
        const next = new Set(prev);
        next.delete(bookPath);
        return next;
      });
      if (!res.ok && res.error) setActionError(res.error);
    },
    [],
  );

  const handleHide = useCallback(
    (b: ScannedBook) => void withBusy(b.path, () => scanStore.hide(client, b)),
    [client, withBusy],
  );
  const handleUnhide = useCallback(
    (b: ScannedBook) => void withBusy(b.path, () => scanStore.unhide(client, b)),
    [client, withBusy],
  );
  const handleClearMatch = useCallback(
    (b: ScannedBook) => void withBusy(b.path, () => scanStore.clearMatch(client, b)),
    [client, withBusy],
  );

  const handlePick = useCallback(
    (result: MetaSearchResult) => {
      const b = matchTarget;
      setMatchTarget(null);
      if (!b) return;
      void withBusy(b.path, () => scanStore.applyManualMatch(client, b, result.id));
    },
    [client, matchTarget, withBusy],
  );

  async function handleProcess() {
    if (selectedVisible.length === 0 || processing) return;
    const outcome = await scanStore.process(client, selectedVisible.map(toCandidate));
    if (outcome.started) onProcessed();
  }

  const scanning = job?.status === 'running' || starting;
  const hasResults = books.length > 0;
  const running = job?.status === 'running';

  return (
    <div className="flex flex-col gap-6">
      <ScanForm
        path={path}
        onPathChange={setPath}
        recent={recent}
        onScan={() => void startScan(path)}
        busy={scanning}
      />

      {scanError && (
        <p role="alert" className="text-sm text-pink-500">
          {scanError}
        </p>
      )}

      {job && <ScanProgressLine job={job} />}
      {actionError && (
        <p role="alert" className="text-sm text-pink-500">
          {actionError}
        </p>
      )}
      {note && (
        <p
          role="status"
          className={'text-sm ' + (note.kind === 'ok' ? 'text-success' : 'text-pink-500')}
        >
          {note.text}
        </p>
      )}

      {job?.status === 'done' && !hasResults && (
        <div className="rounded-xl border border-edge bg-surface p-6 text-sm text-dim">
          No audiobooks were found under that folder.
        </div>
      )}

      {hasResults && (
        <div className="flex flex-col gap-3">
          <Toolbar
            visibleCount={visible.length}
            totalCount={books.length - hidden.length}
            hiddenCount={hidden.length}
            excludeCovered={excludeCovered}
            onExcludeChange={(v) => scanStore.setExcludeCovered(v)}
            showHidden={showHidden}
            onShowHiddenChange={(v) => scanStore.setShowHidden(v)}
            search={search}
            onSearchChange={(v) => scanStore.setSearch(v)}
          />

          {running && (
            <p className="text-xs text-dim">
              Results are still being refined - book identities may update until the scan finishes.
            </p>
          )}

          {gapSeries.length > 0 && (
            <p className="text-xs text-amber-300">
              Series carryover works best in order. Earlier books are unselected in:{' '}
              {gapSeries.join(', ')}.
            </p>
          )}

          <div className="overflow-x-auto rounded-xl border border-edge bg-surface">
            <table className="w-full text-left text-sm">
              <thead className="text-xs uppercase tracking-wide text-dim">
                <tr>
                  <th className="px-3 py-2 font-medium">
                    <input
                      type="checkbox"
                      checked={allVisibleSelected}
                      onChange={(e) =>
                        scanStore.toggleAll(
                          selectableVisible.map((b) => b.path),
                          e.target.checked,
                        )
                      }
                      disabled={selectableVisible.length === 0}
                      aria-label="Select all visible books"
                      className="h-4 w-4 accent-pink-600 disabled:cursor-not-allowed disabled:opacity-40"
                    />
                  </th>
                  <th className="px-3 py-2 font-medium">Book</th>
                  <th className="px-3 py-2 font-medium">Series</th>
                  <th className="px-3 py-2 font-medium">Length</th>
                  <th className="px-3 py-2 font-medium">Coverage</th>
                  <th className="px-3 py-2 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody>
                {visible.map((b) => (
                  <CandidateRow
                    key={b.path}
                    book={b}
                    checked={!b.pipeline_book && selected.has(b.path)}
                    onToggle={handleToggle}
                    onMatch={setMatchTarget}
                    onClearMatch={handleClearMatch}
                    onHide={handleHide}
                    busy={busyPaths.has(b.path)}
                  />
                ))}
              </tbody>
            </table>
          </div>

          {showHidden && hiddenVisible.length > 0 && (
            <div className="flex flex-col gap-2">
              <h3 className="text-xs font-medium uppercase tracking-wide text-dim">
                Hidden ({hiddenVisible.length})
              </h3>
              <div className="overflow-x-auto rounded-xl border border-edge bg-surface">
                <table className="w-full text-left text-sm">
                  <tbody>
                    {hiddenVisible.map((b) => (
                      <CandidateRow
                        key={b.path}
                        book={b}
                        checked={false}
                        onToggle={noopToggle}
                        onUnhide={handleUnhide}
                        busy={busyPaths.has(b.path)}
                      />
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}

          <div className="flex items-center justify-end">
            <button
              type="button"
              onClick={handleProcess}
              disabled={selectedVisible.length === 0 || processing}
              className="rounded-md bg-pink-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-pink-700 disabled:cursor-not-allowed disabled:opacity-50"
            >
              {processing
                ? 'Enqueuing...'
                : `Process ${selectedVisible.length} book${selectedVisible.length === 1 ? '' : 's'}`}
            </button>
          </div>
        </div>
      )}

      {matchTarget && (
        <MatchModal
          client={client}
          book={matchTarget}
          onClose={() => setMatchTarget(null)}
          onPick={handlePick}
        />
      )}
    </div>
  );
}

interface ScanFormProps {
  path: string;
  onPathChange: (v: string) => void;
  recent: string[];
  onScan: () => void;
  busy: boolean;
}

function ScanForm({ path, onPathChange, recent, onScan, busy }: ScanFormProps) {
  const listId = useRef(`recent-roots-${Math.random().toString(36).slice(2)}`).current;
  return (
    <form
      className="rounded-xl border border-edge bg-surface p-6"
      onSubmit={(e) => {
        e.preventDefault();
        onScan();
      }}
    >
      <h2 className="mb-1 text-lg font-medium text-hi">Scan a folder</h2>
      <p className="mb-4 max-w-prose text-sm text-dim">
        Enter the path to an audiobook folder on this machine. The daemon walks it, reads tags, and
        checks each book&apos;s coverage against meta.audiosilo.app.
      </p>
      <div className="flex flex-col gap-3 sm:flex-row">
        <input
          type="text"
          value={path}
          onChange={(e) => onPathChange(e.target.value)}
          list={recent.length > 0 ? listId : undefined}
          placeholder="/path/to/audiobooks"
          aria-label="Folder path"
          className="flex-1 rounded-md border border-edge bg-raised px-3 py-2 font-mono text-sm text-body placeholder:text-dim"
        />
        {recent.length > 0 && (
          <datalist id={listId}>
            {recent.map((r) => (
              <option key={r} value={r} />
            ))}
          </datalist>
        )}
        <button
          type="submit"
          disabled={busy || path.trim() === ''}
          className="rounded-md bg-pink-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-pink-700 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {busy ? 'Scanning...' : 'Scan'}
        </button>
      </div>
    </form>
  );
}

// ScanProgressLine renders a phase-aware status line: folder-walk progress and a
// running book count, then coverage-check progress, then a final summary.
function ScanProgressLine({ job }: { job: ScanJob }) {
  if (job.status === 'error') {
    return (
      <p role="alert" className="text-sm text-pink-500">
        Scan failed: {job.error ?? 'unknown error'}
      </p>
    );
  }

  if (job.status === 'running') {
    return <p className="text-sm text-dim">{runningScanDetail(job.progress)}...</p>;
  }

  const count = job.books.length;
  return (
    <div className="text-sm text-dim">
      <p>
        Scanned {count} book{count === 1 ? '' : 's'} under{' '}
        <span className="font-mono text-body">{job.path}</span>.
      </p>
      <p className="mt-1 text-xs">
        This result is cached across daemon restarts. Scan again after changing files.
      </p>
    </div>
  );
}

interface ToolbarProps {
  visibleCount: number;
  totalCount: number;
  hiddenCount: number;
  excludeCovered: boolean;
  onExcludeChange: (v: boolean) => void;
  showHidden: boolean;
  onShowHiddenChange: (v: boolean) => void;
  search: string;
  onSearchChange: (v: string) => void;
}

function Toolbar({
  visibleCount,
  totalCount,
  hiddenCount,
  excludeCovered,
  onExcludeChange,
  showHidden,
  onShowHiddenChange,
  search,
  onSearchChange,
}: ToolbarProps) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <span className="text-sm text-dim">
        Showing {visibleCount} of {totalCount}
      </span>
      <div className="flex flex-wrap items-center gap-4">
        <input
          type="search"
          value={search}
          onChange={(e) => onSearchChange(e.target.value)}
          placeholder="Search books..."
          aria-label="Search books"
          className="w-48 rounded-md border border-edge bg-raised px-3 py-1.5 text-sm text-body placeholder:text-dim"
        />
        {hiddenCount > 0 && (
          <button
            type="button"
            onClick={() => onShowHiddenChange(!showHidden)}
            className="text-sm text-body underline-offset-2 hover:underline"
          >
            {showHidden ? 'Hide hidden' : `Show hidden (${hiddenCount})`}
          </button>
        )}
        <label className="flex cursor-pointer items-center gap-2 text-sm text-body">
          <input
            type="checkbox"
            checked={excludeCovered}
            onChange={(e) => onExcludeChange(e.target.checked)}
            className="h-4 w-4 accent-pink-600"
          />
          Exclude already covered
        </label>
      </div>
    </div>
  );
}
