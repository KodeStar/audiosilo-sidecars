import type { Coverage } from '@/api/types';
import { coverageState, type CoverageState } from '@/lib/candidates';

const PILL_CLASS: Record<CoverageState, string> = {
  has: 'border-success/40 bg-success/10 text-success',
  needed: 'border-edge bg-raised text-dim',
  unknown: 'border-slate-500/40 bg-slate-500/10 text-slate-300',
  unavailable: 'border-edge bg-raised text-dim',
};

function Pill({ state, label, title }: { state: CoverageState; label: string; title: string }) {
  return (
    <span
      title={title}
      className={
        'inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-medium ' +
        PILL_CLASS[state]
      }
    >
      {state === 'has' && <span aria-hidden="true">&#10003;</span>}
      {label}
    </span>
  );
}

// CoverageBadge renders the per-dimension coverage verdict for a scanned book:
// two pills (Characters / Recaps) when the work is known, or a single book-level
// pill for the unknown / coverage-unavailable cases.
export function CoverageBadge({ coverage }: { coverage: Coverage | undefined }) {
  const chars = coverageState(coverage, 'characters');

  if (chars === 'unavailable') {
    return (
      <Pill
        state="unavailable"
        label="coverage unavailable"
        title="The metadata service was disabled or unreachable during this scan."
      />
    );
  }
  if (chars === 'unknown') {
    return (
      <Pill
        state="unknown"
        label="unknown work"
        title="No ASIN/ISBN matched a known work on meta.audiosilo.app - a good candidate to contribute."
      />
    );
  }

  const recaps = coverageState(coverage, 'recaps');
  return (
    <div className="flex flex-wrap gap-1.5">
      <Pill
        state={chars}
        label="Characters"
        title={
          chars === 'has' ? 'This work already has characters.' : 'Characters not yet contributed.'
        }
      />
      <Pill
        state={recaps}
        label="Recaps"
        title={recaps === 'has' ? 'This work already has recaps.' : 'Recaps not yet contributed.'}
      />
    </div>
  );
}
