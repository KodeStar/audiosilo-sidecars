// Pure merge logic for the Running tab: fold the SSE patch events (book.state,
// stage.progress) into the books list fetched on mount. Kept React-free and
// unit-tested; the panel holds the list in state and applies these on each event.

import type { BookStateEvent, BookView, StageProgressEvent } from '@/api/types';

// applyBookState patches the matching book's state + lane + status + error. An
// event for a book not in the list is ignored (a newly created book arrives via a
// list refetch, which carries its full identity/coverage; SSE only patches what we
// already hold).
export function applyBookState(books: BookView[], ev: BookStateEvent): BookView[] {
  let changed = false;
  const next = books.map((b) => {
    if (b.id !== ev.book_id) return b;
    changed = true;
    return { ...b, state: ev.state, lane: ev.lane, status: ev.status, error: ev.error };
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

// sortBooks orders the list newest-created first, keeping done books last so the
// active work stays at the top of the Running tab.
export function sortBooks(books: BookView[]): BookView[] {
  return [...books].sort((a, b) => {
    const ad = isDone(a) ? 1 : 0;
    const bd = isDone(b) ? 1 : 0;
    if (ad !== bd) return ad - bd;
    // Newest first within each group. created_at is the daemon's fixed-width UTC
    // timestamp (nine fractional digits + Z), so a lexicographic compare is exactly
    // chronological; the id is a stable tiebreak for equal timestamps.
    if (a.created_at !== b.created_at) return a.created_at < b.created_at ? 1 : -1;
    return b.id - a.id;
  });
}
