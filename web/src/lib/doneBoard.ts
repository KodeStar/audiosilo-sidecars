// Pure logic for the Done board: which books are done (and their order) and the
// per-stage cost/timing table rows. Kept React-free and unit-tested; the DonePanel
// + DoneRow components render these. Duration formatting is the shared
// lib/duration formatDuration; a null (uncomputable) span renders "-" at the call
// site below.

import type { BookView, StageRun } from '@/api/types';
import { compareByTimestampDesc } from '@/lib/books';
import { formatDuration } from '@/lib/duration';
import { stateLabel } from '@/lib/pipelineState';
import { parseTimestamp } from '@/lib/time';

// filterDoneBooks keeps only terminal (done) books and orders them newest-finished
// first (by updated_at, id-tiebroken). Returns a new array; does not mutate.
export function filterDoneBooks(books: BookView[]): BookView[] {
  return books
    .filter((b) => b.state === 'done')
    .sort((a, b) => compareByTimestampDesc(a.updated_at, b.updated_at, a.id, b.id));
}

// runDurationSeconds is the wallclock span of one stage run, or null when it cannot
// be computed: a run still in flight (empty finished_at), a missing started_at, an
// unparseable timestamp, or an end before the start (clock skew). The caller shows
// "-" for null.
export function runDurationSeconds(run: StageRun): number | null {
  if (!run.finished_at || !run.started_at) return null;
  const start = parseTimestamp(run.started_at);
  const end = parseTimestamp(run.finished_at);
  if (start === null || end === null || end < start) return null;
  return (end - start) / 1000;
}

// One row of the per-stage cost/timing table. Raw token/cost numbers are kept so
// the component formats them with the shared lib/cost helpers; the label and
// elapsed string are resolved here. failed marks an ok:false run; running marks
// an ok:null run still in flight (rare on a done book, but a resume can leave one).
export interface StageRunRow {
  id: number;
  stage: string;
  model: string;
  inTokens: number;
  outTokens: number;
  cost: number;
  elapsed: string;
  failed: boolean;
  running: boolean;
}

// stageRunRows builds the display rows for a book's stage-run ledger, preserving
// the ledger's order (oldest first). Every run is shown - mechanical/ASR stages
// (zero spend) included - so the table reads as a full timeline, not just agent
// spend.
export function stageRunRows(runs: StageRun[]): StageRunRow[] {
  return runs.map((r) => {
    const dur = runDurationSeconds(r);
    return {
      id: r.id,
      stage: stateLabel(r.stage),
      model: r.model || '-',
      inTokens: r.input_tokens,
      outTokens: r.output_tokens,
      cost: r.cost_usd,
      elapsed: dur === null ? '-' : formatDuration(dur),
      failed: r.ok === false,
      running: r.ok === null,
    };
  });
}

// formatFinishedDate renders a book's finished timestamp as a short human date,
// e.g. "17 Jul 2026, 14:32". Falls back to the raw string when it cannot be
// parsed. Locale-agnostic (fixed en-GB style) so it reads the same everywhere.
export function formatFinishedDate(iso: string): string {
  const ms = parseTimestamp(iso);
  if (ms === null) return iso;
  return new Date(ms).toLocaleString('en-GB', {
    day: 'numeric',
    month: 'short',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
}
