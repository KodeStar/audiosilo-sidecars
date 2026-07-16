// Pure presentation logic for a book's pipeline state and status: its human
// label and the chip/badge styling. The lane a state runs in is served by the
// daemon (bookView.lane / the book.state event), so this module no longer
// mirrors the Go state->lane table - only the display strings are client-side.

// Lane is the served lane token; '' (a waypoint) normalizes to 'none' for
// styling. The daemon is authoritative for which state runs in which lane.
export type Lane = 'asr' | 'agent' | 'mechanical' | 'none';

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

// normalizeLane maps a served lane token to the styling key, treating the empty
// waypoint lane (and anything unrecognized) as 'none'.
export function normalizeLane(lane: string): Lane {
  switch (lane) {
    case 'asr':
    case 'agent':
    case 'mechanical':
      return lane;
    default:
      return 'none';
  }
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

// stateChipClass returns the Tailwind classes for a state chip, coloring by the
// served lane with special cases for the ready and done waypoints. lane is the
// daemon-provided lane token (bookView.lane); '' waypoints fall back to 'none'.
export function stateChipClass(state: string, lane: string): string {
  if (state === 'done') return 'border-success/40 bg-success/10 text-success';
  if (state === 'ready') return 'border-amber-500/40 bg-amber-500/10 text-amber-300';
  return LANE_CHIP[normalizeLane(lane)];
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
