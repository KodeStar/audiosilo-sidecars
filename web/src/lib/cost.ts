// Pure token/cost formatting + per-book aggregation for the Running tab's stage
// ledger. Kept React-free and unit-tested; the row component renders these.

import type { StageRun } from '@/api/types';

// formatTokens renders a token count compactly: <1000 verbatim, thousands as
// "12.3k", millions as "1.2M". A whole thousand/million drops the ".0" (1000 ->
// "1k"). Non-finite or negative input renders "0".
export function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0';
  if (n < 1000) return String(Math.round(n));
  if (n < 1_000_000) return `${trim(n / 1000)}k`;
  return `${trim(n / 1_000_000)}M`;
}

// trim shows one decimal unless the value is whole (12.3 -> "12.3", 1 -> "1").
function trim(v: number): string {
  const rounded = Math.round(v * 10) / 10;
  return Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1);
}

// formatCost renders a USD cost with four decimals ("$0.0123"). A zero cost (a
// mechanical/ASR stage, or a codex run that reports no USD) renders "$0.0000".
export function formatCost(n: number): string {
  const v = Number.isFinite(n) && n > 0 ? n : 0;
  return `$${v.toFixed(4)}`;
}

// hasSpend reports whether a run recorded any agent usage (tokens or cost), i.e.
// it is an agent stage that actually ran, not a mechanical/ASR stage.
export function hasSpend(run: StageRun): boolean {
  return (
    run.input_tokens > 0 ||
    run.output_tokens > 0 ||
    (run.cache_read_tokens ?? 0) > 0 ||
    run.cost_usd > 0 ||
    run.estimated_api_cost_usd !== undefined
  );
}

// agentSpendRuns filters a book's runs to those with recorded agent usage, in the
// ledger's existing order (oldest first). Returns [] for a book with no agent
// spend yet (still in the mechanical/ASR stages).
export function agentSpendRuns(runs: StageRun[]): StageRun[] {
  return runs.filter(hasSpend);
}

// bookTotalCost is the backward-compatible provider-reported subtotal used by older
// cards. Detailed ledgers use bookCostSummary so an unavailable provider figure is
// shown separately from a configured API-equivalent estimate.
export function bookTotalCost(runs: StageRun[]): number {
  return runs.reduce((sum, r) => sum + (Number.isFinite(r.cost_usd) ? r.cost_usd : 0), 0);
}

export function formatRunCost(run: StageRun): string {
  if (run.cost_reported === true || (run.cost_reported === undefined && run.cost_usd > 0)) {
    return `${formatCost(run.cost_usd)} reported`;
  }
  if (run.cost_usd > 0) {
    return `${formatCost(run.cost_usd)} reported (partial)`;
  }
  if (run.estimated_api_cost_usd !== undefined) {
    return `${formatCost(run.estimated_api_cost_usd)} API-equivalent estimate${run.estimate_complete === false ? ' (partial)' : ''}`;
  }
  return hasSpend(run) ? 'cost unavailable' : '-';
}

export interface BookCostSummary {
  reported: number;
  estimated: number;
  reportedIncomplete: boolean;
  estimateIncomplete: boolean;
}

export function bookCostSummary(runs: StageRun[]): BookCostSummary {
  return runs.reduce<BookCostSummary>(
    (total, run) => {
      if (!hasSpend(run)) return total;
      total.reported += Number.isFinite(run.cost_usd) ? run.cost_usd : 0;
      total.estimated += Number.isFinite(run.estimated_api_cost_usd)
        ? (run.estimated_api_cost_usd ?? 0)
        : 0;
      const legacyReported = run.cost_reported === undefined && run.cost_usd > 0;
      total.reportedIncomplete ||= run.cost_reported !== true && !legacyReported;
      total.estimateIncomplete ||=
        run.estimated_api_cost_usd === undefined || run.estimate_complete === false;
      return total;
    },
    { reported: 0, estimated: 0, reportedIncomplete: false, estimateIncomplete: false },
  );
}
