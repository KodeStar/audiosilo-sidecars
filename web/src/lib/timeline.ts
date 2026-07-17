// Pure logic for the Running tab's stage-chip timeline. Given a book's current
// state + status, it returns the ordered mainline pipeline stages each tagged
// done / active / pending, so a row can render a compact progress row. Kept
// React-free and unit-tested; the BookRow renders the chips.

import { titleCaseToken } from '@/lib/pipelineState';

export type TimelineStatus = 'done' | 'active' | 'pending';

export interface TimelineStage {
  stage: string;
  status: TimelineStatus;
}

// The optimistic mainline (the happy path taken at each fork), mirroring the
// M6 ETA remaining-path model: at Inspecting we assume markers are fine, at
// QASweep we assume the transcript is clean, at Auditing we assume the audit
// passes. Off-mainline stages (markers_normalizing, qa_adjudicating,
// retranscribing, fixing) are NOT in this list - they are inserted only when
// the book is currently in one of them.
export const MAINLINE: string[] = [
  'queued',
  'inspecting',
  'splitting',
  'asr',
  'sanitizing',
  'qa_sweep',
  'spelling_research',
  'correcting',
  'fact_pass',
  'synthesizing',
  'validating',
  'auditing',
  'ready',
  'contributing',
  'done',
];

// An off-mainline stage is shown at its natural position: inserted immediately
// after the mainline stage it forks from. Only the book's CURRENT off-mainline
// stage is inserted (state is a single value), so at most one of these appears.
export const OFF_MAINLINE_AFTER: Record<string, string> = {
  markers_normalizing: 'inspecting',
  qa_adjudicating: 'qa_sweep',
  retranscribing: 'qa_sweep',
  fixing: 'auditing',
};

// Compact chip labels (short enough for a wrapped, text-[10px] chip row). Only
// stages whose short label differs from the title-cased-token fallback are listed;
// stages like queued/ready/done render identically via the fallback in
// compactLabel, so they are intentionally omitted here.
export const COMPACT_LABELS: Record<string, string> = {
  inspecting: 'Inspect',
  markers_normalizing: 'Markers',
  splitting: 'Split',
  asr: 'ASR',
  sanitizing: 'Sanitize',
  qa_sweep: 'QA',
  qa_adjudicating: 'Adjudicate',
  retranscribing: 'Re-ASR',
  spelling_research: 'Spelling',
  correcting: 'Correct',
  fact_pass: 'Facts',
  synthesizing: 'Synth',
  validating: 'Validate',
  auditing: 'Audit',
  fixing: 'Fix',
  contributing: 'Contribute',
};

// compactLabel returns the short chip label for a stage, title-casing an
// unmapped token.
export function compactLabel(stage: string): string {
  return COMPACT_LABELS[stage] ?? titleCaseToken(stage);
}

// buildOrder returns the mainline with the current off-mainline stage (if any)
// spliced in at its natural position.
function buildOrder(state: string): string[] {
  const anchor = OFF_MAINLINE_AFTER[state];
  if (!anchor) return MAINLINE;
  const order = [...MAINLINE];
  const idx = order.indexOf(anchor);
  if (idx >= 0) order.splice(idx + 1, 0, state);
  return order;
}

// timelineStages classifies each mainline stage (plus the current off-mainline
// stage) as done / active / pending for the given book state + status.
//
// The active chip is the stage the book is currently at. Everything strictly
// before it in pipeline order is done; everything after is pending. Two resting
// cases where no stage is actively working read as fully/idle-complete: a `done`
// book (every chip done) and a `ready` book that has settled with no status
// (`status === ''` - extraction finished, idle waiting on the M7 contributing
// stage), whose `ready` chip reads done rather than active. An unknown state
// leaves every chip pending. Loops (audit -> fix -> audit, retranscribe -> qa)
// are not predicted - only the current position is shown.
export function timelineStages(state: string, status: string): TimelineStage[] {
  const order = buildOrder(state);
  const activeIdx = state === 'done' ? order.length : order.indexOf(state);
  // The current stage reads as done (not active) when the book is idle-complete:
  // fully done, or settled at the `ready` waypoint with nothing running.
  const currentIsDone = state === 'done' || (state === 'ready' && status === '');
  return order.map((stage, i) => {
    let s: TimelineStatus;
    if (i < activeIdx) s = 'done';
    else if (i === activeIdx) s = currentIsDone ? 'done' : 'active';
    else s = 'pending';
    return { stage, status: s };
  });
}
