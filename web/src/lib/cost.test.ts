import { describe, it, expect } from 'vitest';
import {
  formatTokens,
  formatCost,
  hasSpend,
  agentSpendRuns,
  bookTotalCost,
  bookCostSummary,
  formatRunCost,
} from './cost';
import type { StageRun } from '@/api/types';

function run(partial: Partial<StageRun>): StageRun {
  return {
    id: 1,
    book_id: 1,
    stage: 'fact_pass',
    attempt: 1,
    started_at: '2026-07-17T00:00:00.000000000Z',
    finished_at: '2026-07-17T00:01:00.000000000Z',
    ok: true,
    model: '',
    input_tokens: 0,
    output_tokens: 0,
    cost_usd: 0,
    ...partial,
  };
}

describe('formatTokens', () => {
  it('renders small counts verbatim', () => {
    expect(formatTokens(0)).toBe('0');
    expect(formatTokens(999)).toBe('999');
  });

  it('abbreviates thousands and millions', () => {
    expect(formatTokens(12345)).toBe('12.3k');
    expect(formatTokens(1000)).toBe('1k');
    expect(formatTokens(1_500_000)).toBe('1.5M');
  });

  it('guards non-finite / negative input', () => {
    expect(formatTokens(NaN)).toBe('0');
    expect(formatTokens(-5)).toBe('0');
  });
});

describe('formatCost', () => {
  it('renders four decimals', () => {
    expect(formatCost(0.0123)).toBe('$0.0123');
    expect(formatCost(0)).toBe('$0.0000');
    expect(formatCost(1.5)).toBe('$1.5000');
  });

  it('clamps non-finite / negative to zero', () => {
    expect(formatCost(NaN)).toBe('$0.0000');
    expect(formatCost(-1)).toBe('$0.0000');
  });
});

describe('hasSpend / agentSpendRuns', () => {
  it('detects any recorded token or cost usage', () => {
    expect(hasSpend(run({}))).toBe(false);
    expect(hasSpend(run({ input_tokens: 10 }))).toBe(true);
    expect(hasSpend(run({ cost_usd: 0.01 }))).toBe(true);
  });

  it('filters to agent-spend rows only, preserving order', () => {
    const runs = [
      run({ id: 1, stage: 'splitting' }), // mechanical, no spend
      run({ id: 2, stage: 'fact_pass', input_tokens: 100, output_tokens: 50, cost_usd: 0.02 }),
      run({ id: 3, stage: 'asr' }), // no spend
      run({ id: 4, stage: 'auditing', input_tokens: 200, cost_usd: 0.03 }),
    ];
    expect(agentSpendRuns(runs).map((r) => r.id)).toEqual([2, 4]);
  });
});

describe('bookTotalCost', () => {
  it('sums cost across every run (codex zeros contribute nothing)', () => {
    const runs = [run({ cost_usd: 0.02 }), run({ cost_usd: 0.03 }), run({ cost_usd: 0 })];
    expect(bookTotalCost(runs)).toBeCloseTo(0.05, 10);
  });

  it('is zero for an empty ledger', () => {
    expect(bookTotalCost([])).toBe(0);
  });
});

describe('reported versus estimated costs', () => {
  it('keeps reported and API-equivalent totals separate with completeness', () => {
    const summary = bookCostSummary([
      run({
        input_tokens: 10,
        cost_usd: 0.02,
        cost_reported: true,
        estimated_api_cost_usd: 0.03,
        estimate_complete: true,
      }),
      run({
        input_tokens: 10,
        cost_reported: false,
        estimated_api_cost_usd: 0.01,
        estimate_complete: true,
      }),
    ]);
    expect(summary).toEqual({
      reported: 0.02,
      estimated: 0.04,
      reportedIncomplete: true,
      estimateIncomplete: false,
    });
  });

  it('does not label unreported usage as free', () => {
    expect(formatRunCost(run({ input_tokens: 10, cost_reported: false }))).toBe('cost unavailable');
    expect(
      formatRunCost(
        run({
          input_tokens: 10,
          cost_reported: false,
          estimated_api_cost_usd: 0.012,
          estimate_complete: true,
        }),
      ),
    ).toBe('$0.0120 API-equivalent estimate');
  });
});
