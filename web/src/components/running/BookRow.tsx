import { memo, useCallback, useEffect, useRef, useState } from 'react';
import type { BookDetail, BookEventsResponse, BookView, LoggedEvent } from '@/api/types';
import { availableActions, formatBytes, isDone, type BookAction } from '@/lib/books';
import { formatLogEvent, formatLogTime } from '@/lib/bookLog';
import { formatCost } from '@/lib/cost';
import { formatDuration, formatEta } from '@/lib/duration';
import { parkHint } from '@/lib/parkReasons';
import { normalizeLane, stateChipClass, stateLabel, statusBadge } from '@/lib/pipelineState';
import { parseTimestamp } from '@/lib/time';
import { compactLabel, timelineStages, type TimelineStatus } from '@/lib/timeline';
import { useLazyDetail } from '@/lib/useLazyDetail';
import { StageCostList } from './StageCostList';

interface BookRowProps {
  book: BookView;
  busy: boolean;
  // A monotonically-updated clock (ms) from the panel's single interval, used to
  // compute elapsed time without a per-row timer.
  now: number;
  onAction: (id: number, action: BookAction) => void;
  // Lazily fetches the book's detail (with its stage-run cost ledger) when the row
  // is expanded. Kept as a prop so the row stays decoupled from ApiClient.
  getDetail: (id: number) => Promise<BookDetail>;
  // Lazily fetches the book's durable event log (newest first) for the details
  // expansion. Kept as a prop for the same decoupling.
  getEvents: (id: number, limit?: number) => Promise<BookEventsResponse>;
  // Opens the core (add-work) proposal modal for a book parked core_needed. The
  // panel owns the modal; the row only surfaces the affordance.
  onCompleteCoreProposal: (book: BookView) => void;
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

// The two non-active timeline chip styles. The active chip reuses the state chip
// styling (lane-colored) so it matches the primary state chip above it.
const TIMELINE_CHIP: Record<Exclude<TimelineStatus, 'active'>, string> = {
  done: 'border-success/25 bg-success/5 text-success/70',
  pending: 'border-edge bg-raised/40 text-dim/70',
};

// Throttle for the live-log refetch: at most one refetch per this many ms while
// the details are open.
const LOG_REFETCH_THROTTLE_MS = 2000;

// BookRow is memoized: the Running list re-renders on every SSE patch, but a row
// whose props are unchanged (referential equality on the patched book object)
// should not re-render.
export const BookRow = memo(function BookRow({
  book,
  busy,
  now,
  onAction,
  getDetail,
  getEvents,
  onCompleteCoreProposal,
}: BookRowProps) {
  const done = isDone(book);
  // A book only actively advertises live readouts (a ticking elapsed clock and an
  // ETA) while it is genuinely running - an empty status. A paused/parked/failed
  // book carries a status, so it must not keep showing a stale/ticking countdown
  // until the next eta.update clears it.
  const running = book.status === '';
  const badge = statusBadge(book.status);
  const seriesText =
    book.series && book.series_pos ? `${book.series} #${book.series_pos}` : book.series;
  const stageProgress = activeProgress(book);
  const elapsed = elapsedSeconds(book, now, done);
  const hint =
    book.status === 'needs_attention' && book.park_code ? parkHint(book.park_code) : null;

  // The cost-ledger detail (shared with the Done row): opened lazily, re-fetched on
  // each open. The live log below is a BookRow-specific addition.
  const { expanded, toggle, detail, detailState } = useLazyDetail<BookDetail>(getDetail, book.id);
  const [logEvents, setLogEvents] = useState<LoggedEvent[] | null>(null);
  const lastLogFetch = useRef(0);

  // refetchLog reloads the compact event log (cheap, limit 50). Non-fatal on
  // error - it keeps the prior log rather than blanking the panel.
  const refetchLog = useCallback(async () => {
    lastLogFetch.current = Date.now();
    try {
      const { events } = await getEvents(book.id, 50);
      setLogEvents(events);
    } catch {
      // ignore - the details panel keeps its previous log
    }
  }, [getEvents, book.id]);

  // Refresh the live log when opened, and on each book.state / stage.progress SSE
  // patch for this book while open (book.state and progressKey change on those),
  // throttled to at most one refetch per LOG_REFETCH_THROTTLE_MS.
  const progressKey = book.progress.map((p) => `${p.stage}:${p.done}/${p.total}`).join(',');
  useEffect(() => {
    if (!expanded) return;
    const since = Date.now() - lastLogFetch.current;
    if (since >= LOG_REFETCH_THROTTLE_MS) {
      void refetchLog();
      return;
    }
    const timer = setTimeout(() => void refetchLog(), LOG_REFETCH_THROTTLE_MS - since);
    return () => clearTimeout(timer);
  }, [expanded, book.state, book.status, progressKey, refetchLog]);

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
            {elapsed !== null && running && (
              <span className="text-[11px] text-dim" title="Elapsed since the first stage started">
                {formatDuration(elapsed)} elapsed
              </span>
            )}
            {book.eta_seconds !== undefined && !done && running && (
              <span className="text-[11px] text-dim" title="Estimated time to ready">
                ETA {formatEta(book.eta_seconds)}
              </span>
            )}
            {(book.duration_sec ?? 0) > 0 && (
              <span className="text-[11px] text-dim" title="Total audio length">
                {formatDuration(book.duration_sec ?? 0)} length
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
          {hint && <p className="mt-1 text-xs text-dim">{hint}</p>}
          {book.park_code === 'core_needed' && (
            <button
              type="button"
              onClick={() => onCompleteCoreProposal(book)}
              className="mt-1.5 w-max rounded-md border border-pink-600/50 px-3 py-1 text-xs font-medium text-pink-400 transition-colors hover:border-pink-600 hover:text-hi"
            >
              Complete work proposal
            </button>
          )}
        </div>

        <div className="flex shrink-0 flex-wrap gap-2">
          <button
            type="button"
            aria-expanded={expanded}
            onClick={toggle}
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

      {!done && <StageTimeline state={book.state} status={book.status} lane={book.lane} />}

      {expanded && (
        <div className="flex flex-col gap-3 rounded-lg border border-edge/50 bg-raised/40 p-3">
          {detailState === 'loading' && <p className="text-xs text-dim">Loading details...</p>}
          {detailState === 'error' && (
            <p className="text-xs text-pink-500">Could not load stage details.</p>
          )}
          {detailState === 'idle' && detail && <StageCostList runs={detail.stage_runs} />}
          <BookLog events={logEvents} />
        </div>
      )}
    </div>
  );
});

// StageTimeline renders the compact stage-chip row for an active book. The active
// chip (the current stage) reuses the lane-colored state-chip styling; done and
// pending chips use the muted timeline styles.
function StageTimeline({ state, status, lane }: { state: string; status: string; lane: string }) {
  const stages = timelineStages(state, status);
  return (
    <div className="flex flex-wrap gap-1">
      {stages.map((s) => (
        <span
          key={s.stage}
          className={
            'inline-flex items-center rounded border px-1.5 py-0.5 text-[10px] font-medium ' +
            (s.status === 'active' ? stateChipClass(state, lane) : TIMELINE_CHIP[s.status])
          }
          title={stateLabel(s.stage)}
        >
          {compactLabel(s.stage)}
        </span>
      ))}
    </div>
  );
}

// BookLog renders the per-book event log (newest first) as a compact list.
function BookLog({ events }: { events: LoggedEvent[] | null }) {
  return (
    <div className="flex flex-col gap-1">
      <div className="text-[11px] font-semibold uppercase tracking-wide text-dim">Log</div>
      {events === null ? (
        <p className="text-xs text-dim">Loading log...</p>
      ) : events.length === 0 ? (
        <p className="text-xs text-dim">No log entries yet.</p>
      ) : (
        <ul className="flex flex-col gap-0.5 text-xs">
          {events.map((e) => (
            <li key={e.id} className="flex items-baseline gap-2">
              <span className="shrink-0 font-mono text-[10px] text-dim">{formatLogTime(e.ts)}</span>
              <span className="text-body">{formatLogEvent(e)}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// activeProgress returns the counter for the book's current stage, if any.
function activeProgress(book: BookView): { done: number; total: number } | null {
  const p = book.progress?.find((x) => x.stage === book.state);
  if (!p || p.total <= 0) return null;
  return { done: p.done, total: p.total };
}

// elapsedSeconds returns whole seconds since the book's first stage started, or
// null when there is no start time or the book is terminal.
function elapsedSeconds(book: BookView, now: number, done: boolean): number | null {
  if (done || !book.started_at) return null;
  const start = parseTimestamp(book.started_at);
  if (start === null) return null;
  const secs = (now - start) / 1000;
  return secs >= 0 ? secs : null;
}
