import type { StageRun } from '@/api/types';
import { agentSpendRuns, bookTotalCost, formatCost, formatTokens } from '@/lib/cost';
import { stateLabel } from '@/lib/pipelineState';

// StageCostList renders a book's agent-stage spend: one compact line per stage with
// recorded token/cost usage, plus a book total. Mechanical/ASR stages (zero usage)
// are omitted. Groundwork for the M6 Done board's cost columns.
export function StageCostList({ runs }: { runs: StageRun[] }) {
  const spend = agentSpendRuns(runs);
  if (spend.length === 0) {
    return <p className="text-xs text-dim">No agent spend recorded yet.</p>;
  }
  const total = bookTotalCost(runs);
  return (
    <div className="flex flex-col gap-1 text-xs">
      {spend.map((r) => (
        <div key={r.id} className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
          <span className="min-w-[9rem] text-body">{stateLabel(r.stage)}</span>
          <span className="font-mono text-dim">{r.model || '-'}</span>
          <span className="text-dim">
            {formatTokens(r.input_tokens)} in / {formatTokens(r.output_tokens)} out
          </span>
          <span className="ml-auto font-mono text-body">{formatCost(r.cost_usd)}</span>
        </div>
      ))}
      <div className="mt-1 flex items-baseline justify-between border-t border-edge/50 pt-1">
        <span className="text-dim">Total</span>
        <span className="font-mono font-semibold text-hi">{formatCost(total)}</span>
      </div>
    </div>
  );
}
