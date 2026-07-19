// Pure merge logic for the Running tab: fold the SSE patch events (book.state,
// stage.progress) into the books list fetched on mount. Kept React-free and
// unit-tested; the panel holds the list in state and applies these on each event.

import type {
  BookStateEvent,
  BookView,
  EtaUpdateEvent,
  QueueBucket,
  QueueStatsEvent,
  StageProgressEvent,
} from '@/api/types';
import { MAINLINE, OFF_MAINLINE_AFTER } from '@/lib/timeline';

// applyBookState patches the matching book's state + lane + status + error +
// park_code + retry_at. An event for a book not in the list is ignored (a newly created
// book arrives via a list refetch, which carries its full identity/coverage; SSE
// only patches what we already hold). park_code and retry_at ride along the patch
// (empty/absent on an advance, so a retry/resume clears a prior park reason and any
// stale scheduled re-admit); retry_at falls back to '' when the frame carries none, so
// an advance/clear frame drops a book's previous "retries automatically" instant. Queue
// placement stays until queue.stats replaces it: consecutive stages may retain an
// identical placement, in which case the daemon deduplicates the queue frame.
export function applyBookState(books: BookView[], ev: BookStateEvent): BookView[] {
  let changed = false;
  const next = books.map((b) => {
    if (b.id !== ev.book_id) return b;
    changed = true;
    return {
      ...b,
      state: ev.state,
      lane: ev.lane,
      status: ev.status,
      error: ev.error,
      park_code: ev.park_code,
      retry_at: ev.retry_at ?? '',
    };
  });
  return changed ? next : books;
}

// applyQueueSnapshot replaces every book's scheduler-owned placement from a
// queue.stats frame. Books omitted from the snapshot are exceptional/terminal and
// have any stale placement cleared. Older daemons omit queue_books; in that case
// leave the current list untouched so the frontend's deterministic fallback works.
export function applyQueueSnapshot(books: BookView[], ev: QueueStatsEvent): BookView[] {
  if (!ev.queue_books) return books;
  const placements = new Map(ev.queue_books.map((entry) => [entry.book_id, entry]));
  let changed = false;
  const next = books.map((book) => {
    const entry = placements.get(book.id);
    const group = entry?.group;
    const bucket = entry?.bucket;
    const position = entry?.position;
    const active = entry?.active;
    if (
      book.queue_group === group &&
      book.queue_bucket === bucket &&
      book.queue_position === position &&
      book.queue_active === active
    ) {
      return book;
    }
    changed = true;
    return {
      ...book,
      queue_group: group,
      queue_bucket: bucket,
      queue_position: position,
      queue_active: active,
    };
  });
  return changed ? next : books;
}

// applyQueueStats folds every per-book field from a daemon-wide queue.stats frame
// while preserving object identity for unchanged rows. BookRow is memoized and the
// queue frame always carries queue_books on current daemons, so an unconditional
// spread here would otherwise re-render the entire board on every occupancy change.
export function applyQueueStats(books: BookView[], ev: QueueStatsEvent): BookView[] {
  const placed = applyQueueSnapshot(books, ev);
  if (!ev.agent_invocations_by_book && !ev.series_blocked_by) return placed;

  let changed = false;
  const next = placed.map((book) => {
    const invocations = ev.agent_invocations_by_book
      ? (ev.agent_invocations_by_book[String(book.id)] ?? 0)
      : book.active_agent_invocations;
    const blocker = ev.series_blocked_by
      ? ev.series_blocked_by[String(book.id)]
      : book.series_blocked_by;
    if (
      invocations === book.active_agent_invocations &&
      sameSeriesBlocker(blocker, book.series_blocked_by)
    ) {
      return book;
    }
    changed = true;
    return {
      ...book,
      ...(ev.agent_invocations_by_book ? { active_agent_invocations: invocations } : {}),
      ...(ev.series_blocked_by ? { series_blocked_by: blocker } : {}),
    };
  });
  return changed ? next : placed;
}

function sameSeriesBlocker(a: BookView['series_blocked_by'], b: BookView['series_blocked_by']) {
  return (
    a === b ||
    (!!a && !!b && a.book_id === b.book_id && a.title === b.title && a.series_pos === b.series_pos)
  );
}

// applyEtaUpdate folds the daemon-wide eta.update frame into the list: each
// listed book gets its eta_seconds; a book NOT listed has its eta_seconds cleared
// (it lost its ETA - it parked or reached a terminal/idle state). Unknown book
// ids in the event are ignored. Returns the same array reference when nothing
// changed so a memoized row list can skip re-rendering.
export function applyEtaUpdate(books: BookView[], ev: EtaUpdateEvent): BookView[] {
  const etas = new Map<number, number>(ev.books.map((b) => [b.book_id, b.eta_seconds]));
  let changed = false;
  const next = books.map((b) => {
    const eta = etas.get(b.id); // number, or undefined when not listed
    if (eta === b.eta_seconds) return b; // no change (undefined === undefined too)
    changed = true;
    return { ...b, eta_seconds: eta };
  });
  return changed ? next : books;
}

// applyStageProgress patches (or inserts) the (book, stage) within-stage counter
// so a row can show "chapter 3/12" live. Unknown books are ignored.
export function applyStageProgress(books: BookView[], ev: StageProgressEvent): BookView[] {
  let changed = false;
  const next = books.map((b) => {
    if (b.id !== ev.book_id) return b;
    changed = true;
    const progress = b.progress ? [...b.progress] : [];
    const idx = progress.findIndex((p) => p.stage === ev.stage);
    const entry = { stage: ev.stage, done: ev.done, total: ev.total };
    if (idx >= 0) progress[idx] = entry;
    else progress.push(entry);
    return { ...b, progress };
  });
  return changed ? next : books;
}

// bumpBookEventCount folds a durable book-scoped SSE frame into a per-book counter,
// returning a new record (so a memoized BookRow re-renders on the changed count
// prop). The criterion: it counts the PublishBook frames that append a line to the
// book's durable log - book.state, stage.progress, stage.note, and contrib.update -
// so BookRow's throttled log refetch retriggers on any of them. This matters because
// a long agent stage emits no stage.progress ticks (only stage.note heartbeats) and a
// contrib.update advancing a done book's status changes no book field the row already
// holds; without this the open log would not refresh until the stage completed or the
// row was collapsed/expanded. The counter's value is opaque - only its change matters.
// Bulk/ephemeral frames (eta.update, queue.stats) are NOT counted: they write no
// per-book log line, and eta.update would retrigger every row each dispatch pass. Pure
// + tested.
export function bumpBookEventCount(
  counts: Record<number, number>,
  bookId: number,
): Record<number, number> {
  return { ...counts, [bookId]: (counts[bookId] ?? 0) + 1 };
}

// pruneBookEventCounts drops counter keys for books no longer in the given live-id
// set (books removed by a reload), returning the same reference when nothing changes
// so a functional setState is a no-op. Pure.
export function pruneBookEventCounts(
  counts: Record<number, number>,
  liveIds: Iterable<number>,
): Record<number, number> {
  const live = liveIds instanceof Set ? liveIds : new Set(liveIds);
  const keys = Object.keys(counts);
  if (keys.every((k) => live.has(Number(k)))) return counts;
  const next: Record<number, number> = {};
  for (const k of keys) {
    if (live.has(Number(k))) next[Number(k)] = counts[Number(k)];
  }
  return next;
}

// isDone reports whether a book has reached the terminal state.
export function isDone(book: BookView): boolean {
  return book.state === 'done';
}

// A control action the UI can invoke on a book.
export type BookAction = 'pause' | 'resume' | 'retry' | 'cancel' | 'delete' | 'purge';

// purgeable mirrors the scheduler's PurgeScratch guard: chapters/ may be reclaimed
// only when a book is done, paused, or failed/cancelled (a running book still
// needs them). needs_attention keeps its chapters (a fix/retranscribe may need
// them), matching the daemon (which 409s a needs_attention purge).
function purgeable(book: BookView): boolean {
  return isDone(book) || book.status === 'paused' || book.status === 'failed';
}

// availableActions derives the control set from a book's status/state. The
// scheduler is the source of truth (it 409s an invalid op), so this only keeps
// the UI tidy - it never has to be exhaustive. Mirrors internal/scheduler's
// control semantics (Cancel marks a book failed; Retry clears failed/parked). A
// 'purge' is offered only when the book is purgeable AND actually holds scratch.
export function availableActions(book: BookView): BookAction[] {
  const base = baseActions(book);
  if (purgeable(book) && (book.scratch_bytes ?? 0) > 0) {
    // Insert purge before the destructive delete/cancel so it reads as the milder
    // reclaim option.
    const tail = base[base.length - 1];
    return [...base.slice(0, -1), 'purge', tail];
  }
  return base;
}

function baseActions(book: BookView): BookAction[] {
  if (isDone(book)) return ['delete'];
  switch (book.status) {
    case 'paused':
      return ['resume', 'cancel'];
    case 'failed':
      return ['retry', 'delete'];
    case 'needs_attention':
      return ['retry', 'cancel'];
    default:
      return ['pause', 'cancel'];
  }
}

// formatBytes renders a byte count as a short human string ("0 B", "512 KB",
// "1.2 GB"). Binary units (1024). Kept pure + tested; the Running/Done rows show
// per-book scratch and the daemon-total disk gauge with it.
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  // Whole bytes show no decimal; larger units show one decimal unless it rounds
  // to a whole number.
  const rounded = unit === 0 ? value : Math.round(value * 10) / 10;
  const text = Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1);
  return `${text} ${units[unit]}`;
}

// compareByTimestampDesc orders two records newest-first by a daemon timestamp,
// with the id as a stable descending tiebreak for equal timestamps. The daemon's
// timestamps are fixed-width UTC (nine fractional digits + Z), so a lexicographic
// string compare is exactly chronological. Shared by sortBooks and the Done board's
// filterDoneBooks (both use updated_at).
export function compareByTimestampDesc(aTs: string, bTs: string, aId: number, bId: number): number {
  if (aTs !== bTs) return aTs < bTs ? 1 : -1;
  return bId - aId;
}

export type RunningSectionKey =
  'processing' | 'asr' | 'paused' | 'needs_attention' | 'failed' | 'completed';

export interface RunningBookSection {
  key: RunningSectionKey;
  label: string;
  books: BookView[];
}

const SECTION_LABELS: Record<RunningSectionKey, string> = {
  processing: 'Processing',
  asr: 'ASR',
  paused: 'Paused',
  needs_attention: 'Needs attention',
  failed: 'Failed',
  completed: 'Completed',
};

const SECTION_ORDER: RunningSectionKey[] = [
  'processing',
  'asr',
  'paused',
  'needs_attention',
  'failed',
  'completed',
];

// fallbackQueueGroup supports a brief handoff between book.state and queue.stats,
// and keeps the bundled UI intelligible against an older daemon. The daemon's
// queue_group remains authoritative whenever it is present.
function fallbackQueueGroup(book: BookView): 'processing' | 'asr' {
  const asrIndex = MAINLINE.indexOf('asr');
  const stateIndex = mainlineIndex(book.state);
  if (book.state === 'retranscribing' || (stateIndex >= 0 && stateIndex <= asrIndex)) {
    return 'asr';
  }
  return 'processing';
}

function sectionKey(book: BookView): RunningSectionKey {
  if (isDone(book)) return 'completed';
  // Pause is cooperative. Until the current worker exits, scheduler placement
  // wins so the row remains visibly in "now" while its Paused chip says it is
  // pausing. The next queue.stats frame clears placement and moves it below.
  if (book.queue_active && book.queue_group) return book.queue_group;
  switch (book.status) {
    case 'paused':
      return 'paused';
    case 'needs_attention':
      return 'needs_attention';
    case 'failed':
      return 'failed';
    default:
      return book.queue_group ?? fallbackQueueGroup(book);
  }
}

const QUEUE_BUCKET_ORDER: Record<'processing' | 'asr', QueueBucket[]> = {
  processing: ['agent_active', 'mechanical_active', 'agent', 'mechanical', 'blocked'],
  asr: [
    'transcribing',
    'retranscribing',
    'preparing_agent_active',
    'preparing_mechanical_active',
    'transcription',
    'retranscription',
    'asr_blocked',
    'preparing_agent',
    'preparing_mechanical',
    'preparing',
  ],
};

const QUEUE_BUCKET_LABELS: Record<QueueBucket, string> = {
  agent_active: 'Agent processing now',
  mechanical_active: 'Mechanical processing now',
  agent: 'Agent queue',
  mechanical: 'Mechanical queue',
  blocked: 'Waiting on series',
  transcribing: 'Transcribing now',
  retranscribing: 'Re-transcribing now',
  transcription: 'Transcription queue',
  retranscription: 'Re-transcription queue',
  asr_blocked: 'Waiting on series',
  preparing_agent_active: 'Preparing with agent now',
  preparing_mechanical_active: 'Preparing audio now',
  preparing_agent: 'Agent preparation queue',
  preparing_mechanical: 'Audio preparation queue',
  preparing: 'Preparing for ASR',
};

// queueBucketLabel names the scheduler lane shown above a run of rows. The fallback
// keeps the UI readable during a mixed-version daemon/UI upgrade.
export function queueBucketLabel(book: BookView): string {
  if (book.queue_bucket) return QUEUE_BUCKET_LABELS[book.queue_bucket];
  if (book.queue_active) return book.queue_group === 'asr' ? 'Transcribing now' : 'Processing now';
  return 'Up next';
}

// mainlineIndex maps a book's pipeline state to its position along the optimistic
// MAINLINE. An off-mainline state (markers_normalizing, qa_adjudicating,
// retranscribing, fixing) resolves to the mainline stage it forks from; an unknown
// state sorts as not-started (-1). Reuses the timeline module's stage tables (the
// hand-mirror of the Go state table) so there is a single ordered stage list.
function mainlineIndex(state: string): number {
  const i = MAINLINE.indexOf(state);
  if (i >= 0) return i;
  const anchor = OFF_MAINLINE_AFTER[state];
  return anchor ? MAINLINE.indexOf(anchor) : -1;
}

// sortBooks follows the scheduler's served queue positions: post-ASR Processing
// first, ASR second, then exceptional and completed sections. Within either live
// queue, position already puts current workers before the exact waiting order. The
// older-daemon fallback retains a deterministic active/stage/FIFO order.
export function sortBooks(books: BookView[]): BookView[] {
  return [...books].sort((a, b) => {
    const sectionA = sectionKey(a);
    const sectionB = sectionKey(b);
    const ra = SECTION_ORDER.indexOf(sectionA);
    const rb = SECTION_ORDER.indexOf(sectionB);
    if (ra !== rb) return ra - rb;
    if (sectionA === 'processing' || sectionA === 'asr') {
      const bucketA = a.queue_bucket;
      const bucketB = b.queue_bucket;
      if (bucketA !== undefined || bucketB !== undefined) {
        if (bucketA === undefined) return 1;
        if (bucketB === undefined) return -1;
        const ba = QUEUE_BUCKET_ORDER[sectionA].indexOf(bucketA);
        const bb = QUEUE_BUCKET_ORDER[sectionB as 'processing' | 'asr'].indexOf(bucketB);
        if (ba !== bb) return ba - bb;
      }
      const pa = a.queue_position;
      const pb = b.queue_position;
      if (pa !== undefined || pb !== undefined) {
        if (pa === undefined) return 1;
        if (pb === undefined) return -1;
        if (pa !== pb) return pa - pb;
        return a.id - b.id;
      }
      // Compatibility fallback: a book with known live agent work leads, then
      // furthest-along stage and FIFO id. New daemons always serve positions.
      const aa = (a.active_agent_invocations ?? 0) > 0;
      const ab = (b.active_agent_invocations ?? 0) > 0;
      if (aa !== ab) return aa ? -1 : 1;
      const ia = mainlineIndex(a.state);
      const ib = mainlineIndex(b.state);
      if (ia !== ib) return ib - ia;
      return a.id - b.id;
    }
    // Exceptional/completed sections: newest real change first.
    return compareByTimestampDesc(a.updated_at, b.updated_at, a.id, b.id);
  });
}

// groupRunningBooks returns only non-empty display sections, preserving sortBooks'
// exact order within each. Keeping this pure lets the panel's visual organization
// remain directly tested without coupling the rule to React markup.
export function groupRunningBooks(books: BookView[]): RunningBookSection[] {
  const sorted = sortBooks(books);
  const members = new Map<RunningSectionKey, BookView[]>();
  for (const book of sorted) {
    const key = sectionKey(book);
    const section = members.get(key);
    if (section) section.push(book);
    else members.set(key, [book]);
  }
  return SECTION_ORDER.flatMap((key) => {
    const section = members.get(key);
    return section ? [{ key, label: SECTION_LABELS[key], books: section }] : [];
  });
}
