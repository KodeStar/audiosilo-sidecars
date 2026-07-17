// Pure logic for the Done board: which books are done (and their order) and the
// per-stage cost/timing table rows. Kept React-free and unit-tested; the DonePanel
// + DoneRow components render these. Duration formatting is the shared
// lib/duration formatDuration; a null (uncomputable) span renders "-" at the call
// site below.

import type { BookView, ContributionRow, ContributionSummary, StageRun } from '@/api/types';
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

// ContributionChip is the display model for the Done board's contribution chip:
// the label, an optional link, and whether it needs an attention (warn) tint.
export interface ContributionChip {
  label: string;
  url: string | null;
  attention: boolean;
}

// contributionChip maps a book's aggregate contribution summary to its chip. An
// absent summary (a book with no contribution rows) and any unrecognized status
// both read as the legacy "Local only" chip with no link. Only a `closed` status
// carries the attention tint (a contribution that will not land without help).
export function contributionChip(summary: ContributionSummary | undefined): ContributionChip {
  const url = summary?.url ? summary.url : null;
  switch (summary?.status) {
    case 'submitted':
      return { label: 'Issue open', url, attention: false };
    case 'pr_open':
      return { label: 'PR open', url, attention: false };
    case 'merged':
      return { label: 'Merged', url, attention: false };
    case 'closed':
      return { label: 'Closed', url, attention: true };
    case 'local':
      return { label: 'Local only', url, attention: false };
    default:
      // Absent summary or an unknown status: the legacy local-only presentation.
      return { label: 'Local only', url: null, attention: false };
  }
}

// A short human label for one contribution status, used on the per-kind detail rows.
const STATUS_LABEL: Record<string, string> = {
  submitted: 'issue open',
  pr_open: 'PR open',
  merged: 'merged',
  closed: 'closed',
  local: 'local',
  already_covered: 'already covered',
};

// ContributionRowLine is the display model for one per-kind contribution row in the
// expanded Done details: a label like "characters #123", the best link, a status
// word, and any caveat note.
export interface ContributionRowLine {
  key: string;
  label: string;
  statusLabel: string;
  url: string | null;
  note: string;
}

// contributionRowLines maps the per-kind rows (from the book detail) to display
// lines. The link prefers the row's own url, falling back to the intake PR url when
// only that is known (issue mode before the bot PR opens has just the issue url).
export function contributionRowLines(rows: ContributionRow[] | undefined): ContributionRowLine[] {
  if (!rows) return [];
  return rows.map((r) => {
    const number = r.number > 0 ? ` #${r.number}` : '';
    return {
      key: r.kind,
      label: `${r.kind}${number}`,
      statusLabel: STATUS_LABEL[r.status] ?? r.status,
      url: r.url ? r.url : r.pr_url ? r.pr_url : null,
      note: r.note,
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
