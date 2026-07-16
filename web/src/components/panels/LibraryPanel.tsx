import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type { BookCreateResult, ScanJob, ScannedBook } from '@/api/types';
import { filterCandidates, seriesGapHint, tallyResults, toCandidate } from '@/lib/candidates';
import { addRecentRoot, loadRecentRoots } from '@/lib/recentRoots';
import { CandidateRow } from '../library/CandidateRow';

interface LibraryPanelProps {
  client: ApiClient;
  // Called after at least one book was enqueued, so the shell can switch to the
  // Running tab.
  onProcessed: () => void;
}

const POLL_MS = 700;

export function LibraryPanel({ client, onProcessed }: LibraryPanelProps) {
  const [path, setPath] = useState('');
  const [recent, setRecent] = useState<string[]>(() => loadRecentRoots());
  const [jobId, setJobId] = useState<string | null>(null);
  const [job, setJob] = useState<ScanJob | null>(null);
  const [scanError, setScanError] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);

  const [excludeCovered, setExcludeCovered] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [processing, setProcessing] = useState(false);
  const [note, setNote] = useState<{ kind: 'ok' | 'error'; text: string } | null>(null);

  // Poll the scan job until it reaches a terminal state.
  useEffect(() => {
    if (!jobId) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      try {
        const j = await client.getScan(jobId);
        if (!active) return;
        setJob(j);
        if (j.status === 'running') {
          timer = setTimeout(poll, POLL_MS);
        }
      } catch (err) {
        if (!active) return;
        setScanError(err instanceof ApiError ? err.message : 'Could not read the scan status.');
      }
    };
    void poll();
    return () => {
      active = false;
      if (timer) clearTimeout(timer);
    };
  }, [jobId, client]);

  const startScan = useCallback(
    async (target: string) => {
      const trimmed = target.trim();
      if (trimmed === '' || starting) return;
      setStarting(true);
      setScanError(null);
      setNote(null);
      setJob(null);
      setJobId(null);
      setSelected(new Set());
      try {
        const { job_id } = await client.createScan(trimmed);
        setRecent(addRecentRoot(trimmed));
        setJobId(job_id);
      } catch (err) {
        if (err instanceof ApiError && err.status === 403) {
          setScanError(
            'That path is outside the configured library roots. Set library_roots in the daemon config to allow it.',
          );
        } else if (err instanceof ApiError) {
          setScanError(err.message);
        } else {
          setScanError('Could not reach the daemon.');
        }
      } finally {
        setStarting(false);
      }
    },
    [client, starting],
  );

  const books = useMemo<ScannedBook[]>(() => job?.result?.books ?? [], [job]);
  const visible = useMemo(
    () => filterCandidates(books, { excludeCovered }),
    [books, excludeCovered],
  );
  const selectedVisible = useMemo(
    () => visible.filter((b) => selected.has(b.path)),
    [visible, selected],
  );
  const gapSeries = useMemo(
    () => seriesGapHint(visible, new Set(selectedVisible.map((b) => b.path))),
    [visible, selectedVisible],
  );

  const allVisibleSelected = visible.length > 0 && visible.every((b) => selected.has(b.path));

  function toggleOne(p: string, on: boolean) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (on) next.add(p);
      else next.delete(p);
      return next;
    });
  }

  function toggleAll(on: boolean) {
    setSelected((prev) => {
      const next = new Set(prev);
      for (const b of visible) {
        if (on) next.add(b.path);
        else next.delete(b.path);
      }
      return next;
    });
  }

  async function handleProcess() {
    if (selectedVisible.length === 0 || processing) return;
    setProcessing(true);
    setNote(null);
    try {
      const { results } = await client.createBooks(selectedVisible.map(toCandidate));
      const { created, conflicts, failed } = tallyResults(results);
      setSelected(new Set());
      if (created > 0) {
        onProcessed();
        return;
      }
      setNote({ kind: conflicts > 0 || failed > 0 ? 'error' : 'ok', text: summarize(results) });
    } catch (err) {
      setNote({
        kind: 'error',
        text: err instanceof ApiError ? err.message : 'Could not enqueue the selected books.',
      });
    } finally {
      setProcessing(false);
    }
  }

  const scanning = job?.status === 'running' || starting;

  return (
    <div className="flex flex-col gap-6">
      <ScanForm
        path={path}
        onPathChange={setPath}
        recent={recent}
        onScan={() => startScan(path)}
        busy={scanning}
      />

      {scanError && (
        <p role="alert" className="text-sm text-pink-500">
          {scanError}
        </p>
      )}

      {job && <ScanStatusLine job={job} />}
      {note && (
        <p
          role="status"
          className={'text-sm ' + (note.kind === 'ok' ? 'text-success' : 'text-pink-500')}
        >
          {note.text}
        </p>
      )}

      {job?.status === 'done' &&
        (books.length === 0 ? (
          <div className="rounded-xl border border-edge bg-surface p-6 text-sm text-dim">
            No audiobooks were found under that folder.
          </div>
        ) : (
          <div className="flex flex-col gap-3">
            <Toolbar
              visibleCount={visible.length}
              totalCount={books.length}
              excludeCovered={excludeCovered}
              onExcludeChange={setExcludeCovered}
            />

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
                        onChange={(e) => toggleAll(e.target.checked)}
                        aria-label="Select all visible books"
                        className="h-4 w-4 accent-pink-600"
                      />
                    </th>
                    <th className="px-3 py-2 font-medium">Book</th>
                    <th className="px-3 py-2 font-medium">Series</th>
                    <th className="px-3 py-2 font-medium">Length</th>
                    <th className="px-3 py-2 font-medium">Coverage</th>
                  </tr>
                </thead>
                <tbody>
                  {visible.map((b) => (
                    <CandidateRow
                      key={b.path}
                      book={b}
                      checked={selected.has(b.path)}
                      onToggle={toggleOne}
                    />
                  ))}
                </tbody>
              </table>
            </div>

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
        ))}
    </div>
  );
}

function summarize(results: BookCreateResult[]): string {
  const { created, conflicts, failed } = tallyResults(results);
  const parts: string[] = [];
  if (created > 0) parts.push(`${created} enqueued`);
  if (conflicts > 0) parts.push(`${conflicts} already enqueued`);
  if (failed > 0) parts.push(`${failed} failed`);
  return parts.length > 0 ? parts.join(', ') + '.' : 'Nothing to enqueue.';
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

function ScanStatusLine({ job }: { job: ScanJob }) {
  if (job.status === 'error') {
    return (
      <p role="alert" className="text-sm text-pink-500">
        Scan failed: {job.error ?? 'unknown error'}
      </p>
    );
  }
  const { phase, done, total } = job.progress;
  if (job.status === 'running') {
    const detail =
      phase === 'coverage' && total > 0
        ? `checking coverage ${done}/${total}`
        : 'walking the folder...';
    return <p className="text-sm text-dim">Scanning - {detail}</p>;
  }
  return (
    <p className="text-sm text-dim">
      Scanned {job.result?.books.length ?? 0} book
      {(job.result?.books.length ?? 0) === 1 ? '' : 's'} under{' '}
      <span className="font-mono text-body">{job.result?.root ?? job.path}</span>.
    </p>
  );
}

interface ToolbarProps {
  visibleCount: number;
  totalCount: number;
  excludeCovered: boolean;
  onExcludeChange: (v: boolean) => void;
}

function Toolbar({ visibleCount, totalCount, excludeCovered, onExcludeChange }: ToolbarProps) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <span className="text-sm text-dim">
        Showing {visibleCount} of {totalCount}
      </span>
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
  );
}
