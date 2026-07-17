import type { StageRun } from '@/api/types';
import { bookTotalCost, formatCost, formatTokens } from '@/lib/cost';
import { stageRunRows } from '@/lib/doneBoard';

// StageCostTable renders a book's full stage-run ledger as a table: one row per
// execution with stage, model, in/out tokens, cost and elapsed time, and a Total
// (cost) footer. Every run is shown (mechanical/ASR stages included) so it reads
// as a timeline; agent stages carry the token/cost figures. Thin over lib/doneBoard
// (row building + elapsed) and lib/cost (token/cost formatting).
export function StageCostTable({ runs }: { runs: StageRun[] }) {
  if (runs.length === 0) {
    return <p className="text-xs text-dim">No stage runs recorded.</p>;
  }
  const rows = stageRunRows(runs);
  const total = bookTotalCost(runs);
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[34rem] text-left text-xs">
        <thead>
          <tr className="border-b border-edge/60 text-[0.65rem] uppercase tracking-wide text-dim">
            <th className="py-1.5 pr-3 font-medium">Stage</th>
            <th className="py-1.5 pr-3 font-medium">Model</th>
            <th className="py-1.5 pr-3 text-right font-medium">Tokens (in/out)</th>
            <th className="py-1.5 pr-3 text-right font-medium">Cost</th>
            <th className="py-1.5 text-right font-medium">Elapsed</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id} className="border-b border-edge/30 last:border-0">
              <td className="py-1.5 pr-3 text-body">
                <span className="flex items-center gap-1.5">
                  {r.stage}
                  {r.failed && (
                    <span
                      className="rounded-full border border-pink-500/40 bg-pink-500/10 px-1.5 py-0.5 text-[0.6rem] font-medium text-pink-400"
                      title="This run failed"
                    >
                      failed
                    </span>
                  )}
                  {r.running && (
                    <span
                      className="rounded-full border border-amber-500/40 bg-amber-500/10 px-1.5 py-0.5 text-[0.6rem] font-medium text-amber-300"
                      title="This run did not finish"
                    >
                      running
                    </span>
                  )}
                </span>
              </td>
              <td className="py-1.5 pr-3 font-mono text-dim">{r.model}</td>
              <td className="py-1.5 pr-3 text-right text-dim">
                {formatTokens(r.inTokens)} / {formatTokens(r.outTokens)}
              </td>
              <td className="py-1.5 pr-3 text-right font-mono text-body">{formatCost(r.cost)}</td>
              <td className="py-1.5 text-right font-mono text-dim">{r.elapsed}</td>
            </tr>
          ))}
        </tbody>
        <tfoot>
          <tr className="border-t border-edge/60">
            <td className="py-1.5 pr-3 font-medium text-dim" colSpan={3}>
              Total
            </td>
            <td className="py-1.5 pr-3 text-right font-mono font-semibold text-hi">
              {formatCost(total)}
            </td>
            <td />
          </tr>
        </tfoot>
      </table>
    </div>
  );
}
