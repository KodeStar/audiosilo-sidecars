import { memo, useCallback, useEffect, useRef, useState } from 'react';
import type {
  BookDetail,
  BookEventsResponse,
  BookView,
  LoggedEvent,
  SupervisorRun,
} from '@/api/types';
import { availableActions, formatBytes, isDone, type BookAction } from '@/lib/books';
import { fetchAllEvents, formatLogEvent, formatLogTime, logToText } from '@/lib/bookLog';
import { formatCost } from '@/lib/cost';
import { triggerBlobDownload } from '@/lib/download';
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
  // Opaque counter bumped on each durable book-scoped SSE frame; retriggers the log
  // refetch effect below.
  eventCount: number;
  onAction: (id: number, action: BookAction) => void;
  // Lazily fetches the book's detail (with its stage-run cost ledger) when the row
  // is expanded. Kept as a prop so the row stays decoupled from ApiClient.
  getDetail: (id: number) => Promise<BookDetail>;
  // Lazily fetches the book's durable event log (newest first) for the details
  // expansion. Kept as a prop for the same decoupling. beforeId pages back through
  // the history (the Download-log control walks the whole backlog).
  getEvents: (id: number, limit?: number, beforeId?: number) => Promise<BookEventsResponse>;
  // Opens the core (add-work) proposal modal for a book parked core_needed. The
  // panel owns the modal; the row only surfaces the affordance.
  onCompleteCoreProposal: (book: BookView) => void;
  onAskSupervisor: (book: BookView) => Promise<void>;
  modelSupervisorEnabled: boolean;
  supervisorIncident?: SupervisorRun;
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
  eventCount,
  onAction,
  getDetail,
  getEvents,
  onCompleteCoreProposal,
  onAskSupervisor,
  modelSupervisorEnabled,
  supervisorIncident,
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
  const postAsr = postASRSeconds(book, now, done);
  const hint =
    book.status === 'needs_attention' && book.park_code
      ? parkHint(book.park_code, !!book.retry_at)
      : null;

  // The cost-ledger detail (shared with the Done row): opened lazily, re-fetched on
  // each open. The live log below is a BookRow-specific addition.
  const { expanded, toggle, detail, detailState } = useLazyDetail<BookDetail>(getDetail, book.id);
  const [logEvents, setLogEvents] = useState<LoggedEvent[] | null>(null);
  const [asking, setAsking] = useState(false);
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

  // Refresh the live log when opened, and while open on any change that may have added
  // a log line, throttled to at most one refetch per LOG_REFETCH_THROTTLE_MS. eventCount
  // covers every durable book-scoped SSE frame (the main trigger, incl. long agent
  // stages that emit only stage.note); book.state/book.status/progressKey cover
  // load()-driven full reloads (which do not go through the SSE counter).
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
  }, [expanded, book.state, book.status, progressKey, eventCount, refetchLog]);

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
            {book.series_blocked_by && (
              <span
                className="inline-flex items-center rounded-full border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 text-[11px] font-medium text-amber-300"
                title={`${book.series_blocked_by.title} must reach Ready before this agent stage can start.`}
              >
                Waiting for earlier series book: {book.series_blocked_by.title}
                {book.series_blocked_by.series_pos ? ` #${book.series_blocked_by.series_pos}` : ''}
              </span>
            )}
            {stageProgress && (
              <span className="text-[11px] text-dim">
                {stageProgress.done}/{stageProgress.total}
              </span>
            )}
            {postAsr !== null && (
              <span
                className="text-[11px] text-body"
                title="Elapsed since successful primary ASR completion"
              >
                {formatDuration(postAsr)} post-ASR
              </span>
            )}
            {elapsed !== null && running && (
              <span
                className="text-[11px] text-dim"
                title="End-to-end elapsed since the first batch stage started"
              >
                {formatDuration(elapsed)} batch elapsed
              </span>
            )}
            {(book.active_agent_invocations ?? 0) > 0 && (
              <span className="text-[11px] text-dim">
                {book.active_agent_invocations}
                {book.max_agents_per_book ? `/${book.max_agents_per_book}` : ''} active agent
                invocation{book.active_agent_invocations === 1 ? '' : 's'}
              </span>
            )}
            {book.fanout_supported && (book.current_work_units ?? 0) > 0 && (
              <span
                className="text-[11px] text-dim"
                title="This stage supports isolated per-book fan-out"
              >
                fan-out {book.completed_work_units ?? 0}/{book.current_work_units} units
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
              <span
                className="text-[11px] text-dim"
                title="Provider-reported agent spend; details may also include API-equivalent estimates"
              >
                {formatCost(book.total_cost_usd)} reported
              </span>
            )}
          </div>
          {book.error && book.status !== 'paused' && (
            <p className="mt-1 text-xs text-pink-500">{book.error}</p>
          )}
          {hint && <p className="mt-1 text-xs text-dim">{hint}</p>}
          {book.status === 'needs_attention' &&
            book.park_code?.startsWith('supervisor_') &&
            supervisorIncident && (
              <div className="mt-2 rounded-md border border-amber-500/30 bg-amber-500/5 p-2 text-xs text-body">
                <p>
                  <span className="font-semibold text-amber-300">Supervisor diagnosis:</span>{' '}
                  {supervisorIncident.diagnosis}
                </p>
                <p className="mt-1 text-dim">
                  Action: {supervisorIncident.selected_action}
                  {supervisorIncident.action_outcome
                    ? ` — ${supervisorIncident.action_outcome}`
                    : ''}
                </p>
                {supervisorIncident.evidence.length > 0 && (
                  <div className="mt-1">
                    <span className="font-medium text-body">Evidence:</span>
                    <ul className="mt-0.5 list-inside list-disc">
                      {supervisorIncident.evidence.map((item, index) => (
                        <li key={`${index}-${item}`} className="break-all text-dim">
                          {item}
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
              </div>
            )}
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
          {modelSupervisorEnabled && !done && (
            <button
              type="button"
              disabled={asking || busy}
              onClick={() => {
                setAsking(true);
                void onAskSupervisor(book).finally(() => setAsking(false));
              }}
              className="rounded-md border border-edge px-3 py-1.5 text-xs font-medium text-body transition-colors hover:border-pink-600 hover:text-hi disabled:opacity-50"
            >
              {asking ? 'Asking...' : 'Ask supervisor'}
            </button>
          )}
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
          {detailState === 'idle' && detail?.timing && <TimingBreakdown book={detail} />}
          <BookLog events={logEvents} bookId={book.id} title={book.title} getEvents={getEvents} />
        </div>
      )}
    </div>
  );
});

function TimingBreakdown({ book }: { book: BookView }) {
  const t = book.timing;
  if (!t) return null;
  const rows = [
    ['Batch elapsed', t.batch_elapsed_seconds],
    ['Pre-ASR wall', t.pre_asr_wall_seconds],
    ['ASR active', t.asr_active_seconds],
    ['Post-ASR elapsed', t.post_asr_elapsed_seconds],
    ['Active processing', t.active_processing_seconds],
    ['Queue/wait', t.queue_wait_seconds],
  ] as const;
  return (
    <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs">
      {rows.map(([label, value]) => (
        <div key={label} className="contents">
          <dt className="text-dim">{label}</dt>
          <dd className="text-body">
            {value === undefined ? 'Not available' : formatDuration(value)}
          </dd>
        </div>
      ))}
    </dl>
  );
}

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

// BookLog renders the per-book event log (newest first) in a bounded, scrollable
// panel, with a Download control that pages back through the WHOLE history (the live
// list only holds the newest window) and saves it as a chronological .txt.
function BookLog({
  events,
  bookId,
  title,
  getEvents,
}: {
  events: LoggedEvent[] | null;
  bookId: number;
  title: string;
  getEvents: (id: number, limit?: number, beforeId?: number) => Promise<BookEventsResponse>;
}) {
  const [downloading, setDownloading] = useState(false);
  const [downloadError, setDownloadError] = useState(false);

  const download = useCallback(async () => {
    setDownloading(true);
    setDownloadError(false);
    try {
      const all = await fetchAllEvents(getEvents, bookId);
      const header = `Log for: ${title}\n\n`;
      const blob = new Blob([header + logToText(all)], { type: 'text/plain;charset=utf-8' });
      triggerBlobDownload(blob, `audiosilo-${bookId}-log.txt`);
    } catch {
      setDownloadError(true);
    } finally {
      setDownloading(false);
    }
  }, [getEvents, bookId, title]);

  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-center justify-between gap-2">
        <div className="text-[11px] font-semibold uppercase tracking-wide text-dim">Log</div>
        <button
          type="button"
          disabled={downloading}
          onClick={() => void download()}
          className="rounded-md border border-edge px-2 py-0.5 text-[11px] font-medium text-body transition-colors hover:border-pink-600 hover:text-hi disabled:cursor-not-allowed disabled:opacity-50"
        >
          {downloading ? 'Preparing...' : 'Download log'}
        </button>
      </div>
      {downloadError && <p className="text-xs text-pink-500">Could not download the full log.</p>}
      {events === null ? (
        <p className="text-xs text-dim">Loading log...</p>
      ) : events.length === 0 ? (
        <p className="text-xs text-dim">No log entries yet.</p>
      ) : (
        <ul className="flex max-h-64 flex-col gap-0.5 overflow-y-auto text-xs">
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

function postASRSeconds(book: BookView, now: number, done: boolean): number | null {
  const baseline = book.timing?.primary_asr_completed_at;
  if (!baseline) return null;
  if (done) return book.timing?.post_asr_elapsed_seconds ?? null;
  const start = parseTimestamp(baseline);
  if (start === null) return null;
  return Math.max(0, (now - start) / 1000);
}
