// Pure presentation logic for a book's pipeline state and status: the lane a
// state runs in, its human label, and the chip/badge styling. Mirrors the Go
// state table (internal/state/state.go) by hand - keep them in sync.

export type Lane = 'asr' | 'agent' | 'mechanical' | 'none';

// LANES mirrors internal/state's table (which lane executes each state).
const LANES: Record<string, Lane> = {
  queued: 'none',
  inspecting: 'mechanical',
  markers_normalizing: 'agent',
  splitting: 'mechanical',
  asr: 'asr',
  sanitizing: 'mechanical',
  qa_sweep: 'mechanical',
  qa_adjudicating: 'agent',
  retranscribing: 'asr',
  spelling_research: 'agent',
  correcting: 'mechanical',
  fact_pass: 'agent',
  synthesizing: 'agent',
  validating: 'mechanical',
  auditing: 'agent',
  fixing: 'agent',
  ready: 'none',
  contributing: 'mechanical',
  done: 'none',
};

const LABELS: Record<string, string> = {
  queued: 'Queued',
  inspecting: 'Inspecting',
  markers_normalizing: 'Normalizing markers',
  splitting: 'Splitting',
  asr: 'Transcribing',
  sanitizing: 'Sanitizing',
  qa_sweep: 'QA sweep',
  qa_adjudicating: 'QA adjudicating',
  retranscribing: 'Retranscribing',
  spelling_research: 'Spelling research',
  correcting: 'Correcting',
  fact_pass: 'Fact pass',
  synthesizing: 'Synthesizing',
  validating: 'Validating',
  auditing: 'Auditing',
  fixing: 'Fixing',
  ready: 'Ready',
  contributing: 'Contributing',
  done: 'Done',
};

// laneOf returns the lane a pipeline state runs in ('none' for waypoints and
// unknown states).
export function laneOf(state: string): Lane {
  return LANES[state] ?? 'none';
}

// stateLabel returns a human label for a pipeline state, falling back to a
// title-cased version of the raw token for a state we have not mapped.
export function stateLabel(state: string): string {
  const known = LABELS[state];
  if (known) return known;
  return state
    .split('_')
    .map((w) => (w.length > 0 ? w[0].toUpperCase() + w.slice(1) : w))
    .join(' ');
}

// Chip styling keyed by lane, plus the distinct waypoint colors. Uses Tailwind
// default palette shades alongside the brand tokens (pink/success/edge).
const LANE_CHIP: Record<Lane, string> = {
  asr: 'border-sky-500/40 bg-sky-500/10 text-sky-300',
  agent: 'border-pink-500/40 bg-pink-500/10 text-pink-300',
  mechanical: 'border-slate-500/40 bg-slate-500/10 text-slate-300',
  none: 'border-edge bg-raised text-dim',
};

// stateChipClass returns the Tailwind classes for a state chip, coloring by lane
// with special cases for the ready and done waypoints.
export function stateChipClass(state: string): string {
  if (state === 'done') return 'border-success/40 bg-success/10 text-success';
  if (state === 'ready') return 'border-amber-500/40 bg-amber-500/10 text-amber-300';
  return LANE_CHIP[laneOf(state)];
}

export interface StatusBadge {
  label: string;
  className: string;
}

// statusBadge returns the badge for an exceptional status, or null for the
// normal running condition ('' / 'none').
export function statusBadge(status: string): StatusBadge | null {
  switch (status) {
    case 'paused':
      return { label: 'Paused', className: 'border-amber-500/40 bg-amber-500/10 text-amber-300' };
    case 'needs_attention':
      return {
        label: 'Needs attention',
        className: 'border-orange-500/40 bg-orange-500/10 text-orange-300',
      };
    case 'failed':
      return { label: 'Failed', className: 'border-pink-500/40 bg-pink-500/10 text-pink-300' };
    default:
      return null;
  }
}
