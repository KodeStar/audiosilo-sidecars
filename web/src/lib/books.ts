// Pure merge logic for the Running tab: fold the SSE patch events (book.state,
// stage.progress) into the books list fetched on mount. Kept React-free and
// unit-tested; the panel holds the list in state and applies these on each event.

import type { BookStateEvent, BookView, EtaUpdateEvent, StageProgressEvent } from '@/api/types';
import { MAINLINE, OFF_MAINLINE_AFTER } from '@/lib/timeline';

// applyBookState patches the matching book's state + lane + status + error +
// park_code + retry_at. An event for a book not in the list is ignored (a newly created
// book arrives via a list refetch, which carries its full identity/coverage; SSE
// only patches what we already hold). park_code and retry_at ride along the patch
// (empty/absent on an advance, so a retry/resume clears a prior park reason and any
// stale scheduled re-admit); retry_at falls back to '' when the frame carries none, so
// an advance/clear frame drops a book's previous "retries automatically" instant.
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
// string compare is exactly chronological. Shared by sortBooks (created_at) and the
// Done board's filterDoneBooks (updated_at).
export function compareByTimestampDesc(aTs: string, bTs: string, aId: number, bId: number): number {
  if (aTs !== bTs) return aTs < bTs ? 1 : -1;
  return bId - aId;
}

// bucketRank groups a book for the Running list's top-to-bottom order (lower sorts
// higher): the active/current book(s) first, then the ready queue, then paused,
// parked (needs_attention), failed, and done last. isDone is checked first so a
// finished book (status '', empty lane) never falls into the queued bucket.
function bucketRank(book: BookView): number {
  if (isDone(book)) return 5;
  switch (book.status) {
    case 'paused':
      return 2;
    case 'needs_attention':
      return 3;
    case 'failed':
      return 4;
    default:
      // status === '' : on a worker now (non-empty lane) = active, else queued/ready.
      return book.lane ? 0 : 1;
  }
}

// mainlineIndex maps a book's pipeline state to its position along the optimistic
// MAINLINE (higher = further along), so the active bucket can put the
// furthest-along ("current") book at the very top. An off-mainline state
// (markers_normalizing, qa_adjudicating, retranscribing, fixing) resolves to the
// mainline stage it forks from; an unknown state sorts as not-started (-1). Reuses
// the timeline module's stage tables (the hand-mirror of the Go state table) so
// there is a single ordered stage list.
function mainlineIndex(state: string): number {
  const i = MAINLINE.indexOf(state);
  if (i >= 0) return i;
  const anchor = OFF_MAINLINE_AFTER[state];
  return anchor ? MAINLINE.indexOf(anchor) : -1;
}

// sortBooks orders the Running list so the active/current book is on top, then the
// queue (FIFO: the next-to-run right under it), then paused, parked, and failed
// books, with done books last. Within the active bucket the furthest-along book
// sorts first (tie-broken by newest start, then id); the other non-queue buckets
// keep a stable newest-change-first order. Total and deterministic.
export function sortBooks(books: BookView[]): BookView[] {
  return [...books].sort((a, b) => {
    const ra = bucketRank(a);
    const rb = bucketRank(b);
    if (ra !== rb) return ra - rb;
    if (ra === 0) {
      // Active: furthest along the pipeline first, then newest start, then id.
      const ia = mainlineIndex(a.state);
      const ib = mainlineIndex(b.state);
      if (ia !== ib) return ib - ia;
      return compareByTimestampDesc(a.started_at ?? '', b.started_at ?? '', a.id, b.id);
    }
    if (ra === 1) {
      // Queued: FIFO - oldest created first, so the next-to-run sits at the top of
      // the group right under the active book. Ascending id is the stable tiebreak.
      if (a.created_at !== b.created_at) return a.created_at < b.created_at ? -1 : 1;
      return a.id - b.id;
    }
    // Paused / parked / failed / done: newest real change first, id as the tiebreak.
    return compareByTimestampDesc(a.updated_at, b.updated_at, a.id, b.id);
  });
}
