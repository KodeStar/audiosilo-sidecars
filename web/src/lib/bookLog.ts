// Pure formatting for the Running tab's per-book event log (GET /books/{id}/events).
// Turns a durable event-log row into a one-line human summary. Kept React-free
// and unit-tested; the BookRow renders the time-of-day + this summary.

import type { LoggedEvent } from '@/api/types';
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
    default:
      return ev.type;
  }
}

// formatLogTime renders the event timestamp as a local time-of-day, or '' when
// the timestamp cannot be parsed.
export function formatLogTime(ts: string): string {
  const ms = parseTimestamp(ts);
  if (ms === null) return '';
  return new Date(ms).toLocaleTimeString();
}
