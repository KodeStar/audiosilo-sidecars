// Pure formatting for the Running tab's per-book event log (GET /books/{id}/events).
// Turns a durable event-log row into a one-line human summary. Kept React-free
// and unit-tested; the BookRow renders the time-of-day + this summary.

import type { BookEventsResponse, LoggedEvent } from '@/api/types';
import { parseTimestamp } from '@/lib/time';

// asRecord narrows an unknown payload to a string-keyed object for safe reads.
function asRecord(v: unknown): Record<string, unknown> {
  return typeof v === 'object' && v !== null ? (v as Record<string, unknown>) : {};
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

function num(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : 0;
}

function strings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((item): item is string => typeof item === 'string') : [];
}

// formatLogEvent renders a one-line summary for a logged event. Recognized types
// (book.state, stage.progress) get a human phrasing; anything else falls back to
// the raw event type so an unknown/new event is still shown.
export function formatLogEvent(ev: LoggedEvent): string {
  const p = asRecord(ev.payload);
  switch (ev.type) {
    case 'book.state': {
      const state = str(p.state) || '?';
      const status = str(p.status);
      const error = str(p.error);
      let out = `state -> ${state}`;
      if (status) out += ` (${status}${error ? `: ${error}` : ''})`;
      return out;
    }
    case 'stage.progress': {
      const stage = str(p.stage) || '?';
      return `${stage} ${num(p.done)}/${num(p.total)}`;
    }
    case 'stage.note': {
      // A human-readable stage line ("normalizing markers over 3 chapters",
      // heartbeats, "re-transcribing 2 chapters: 2, 3"). Prefix the stage unless the
      // message already leads with it (some notes, like the heartbeat, embed it).
      const stage = str(p.stage);
      const msg = str(p.msg);
      if (!msg) return ev.type;
      if (!stage || msg.startsWith(`${stage}:`)) return msg;
      return `${stage}: ${msg}`;
    }
    case 'supervisor.decision': {
      const diagnosis = str(p.diagnosis) || 'no diagnosis supplied';
      const action = str(p.selected_action);
      const evidence = strings(p.evidence);
      let out = `supervisor: ${diagnosis}`;
      if (action) out += ` -> ${action}`;
      if (evidence.length > 0) out += `; evidence: ${evidence.join('; ')}`;
      return out;
    }
    default:
      return ev.type;
  }
}

// getEventsPage fetches one page of a book's events (newest first). A beforeId cursor
// returns only older events, so fetchAllEvents can page back through the whole log.
type getEventsPage = (id: number, limit?: number, beforeId?: number) => Promise<BookEventsResponse>;

// Per-page size for the full-history download and the cap on how many pages we walk
// (a guard against an unexpected non-shrinking response - the 30-day prune keeps the
// real backlog far under 500 * this).
const DOWNLOAD_PAGE_SIZE = 500;
const MAX_DOWNLOAD_PAGES = 1000;

// fetchAllEvents pages back through a book's entire durable log via the before_id
// cursor and returns the events in CHRONOLOGICAL order (oldest first). Each page is
// newest-first; we walk older with the oldest id seen so far and stop once a page is
// short (fewer than a full page remain).
export async function fetchAllEvents(
  getEvents: getEventsPage,
  bookId: number,
): Promise<LoggedEvent[]> {
  const all: LoggedEvent[] = [];
  let beforeId: number | undefined;
  for (let page = 0; page < MAX_DOWNLOAD_PAGES; page++) {
    const { events } = await getEvents(bookId, DOWNLOAD_PAGE_SIZE, beforeId);
    all.push(...events);
    if (events.length < DOWNLOAD_PAGE_SIZE) break;
    // Page older than the oldest (last, since newest-first) id we have.
    beforeId = events[events.length - 1].id;
  }
  // Pages accumulated newest-first across the whole set; reverse to chronological.
  all.reverse();
  return all;
}

// logToText renders events (in the given order) as a plain-text log, one line each:
// "<time>  <summary>". Used for the downloadable .txt. A trailing newline is added
// when there is content so the file ends cleanly.
export function logToText(events: LoggedEvent[]): string {
  if (events.length === 0) return '';
  return (
    events
      .map((e) => {
        const t = formatLogTime(e.ts);
        return `${t ? `${t}  ` : ''}${formatLogEvent(e)}`;
      })
      .join('\n') + '\n'
  );
}

// formatLogTime renders the event timestamp as a local time-of-day, or '' when
// the timestamp cannot be parsed.
export function formatLogTime(ts: string): string {
  const ms = parseTimestamp(ts);
  if (ms === null) return '';
  return new Date(ms).toLocaleTimeString();
}
