// Pure merge logic for the Running tab: fold the SSE patch events (book.state,
// stage.progress) into the books list fetched on mount. Kept React-free and
// unit-tested; the panel holds the list in state and applies these on each event.

import type { BookStateEvent, BookView, StageProgressEvent } from '@/api/types';

// applyBookState patches the matching book's state + status. An event for a book
// not in the list is ignored (a newly created book arrives via a list refetch,
// which carries its full identity/coverage; SSE only patches what we already hold).
export function applyBookState(books: BookView[], ev: BookStateEvent): BookView[] {
  let changed = false;
  const next = books.map((b) => {
    if (b.id !== ev.book_id) return b;
    changed = true;
    return { ...b, state: ev.state, lane: ev.lane, status: ev.status };
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
export type BookAction = 'pause' | 'resume' | 'retry' | 'cancel' | 'delete';

// availableActions derives the control set from a book's status/state. The
// scheduler is the source of truth (it 409s an invalid op), so this only keeps
// the UI tidy - it never has to be exhaustive. Mirrors internal/scheduler's
// control semantics (Cancel marks a book failed; Retry clears failed/parked).
export function availableActions(book: BookView): BookAction[] {
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

// sortBooks orders the list newest-created first, keeping done books last so the
// active work stays at the top of the Running tab.
export function sortBooks(books: BookView[]): BookView[] {
  return [...books].sort((a, b) => {
    const ad = isDone(a) ? 1 : 0;
    const bd = isDone(b) ? 1 : 0;
    if (ad !== bd) return ad - bd;
    // Newest first within each group (created_at is an RFC3339 string, so a
    // lexicographic compare is chronological).
    if (a.created_at !== b.created_at) return a.created_at < b.created_at ? 1 : -1;
    return b.id - a.id;
  });
}
