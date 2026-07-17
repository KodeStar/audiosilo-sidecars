import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { StageCostList } from './StageCostList';
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

describe('StageCostList', () => {
  it('renders a line per agent-spend stage plus a total', () => {
    const runs = [
      run({ id: 1, stage: 'splitting' }), // mechanical, omitted
      run({
        id: 2,
        stage: 'fact_pass',
        model: 'sonnet',
        input_tokens: 12345,
        output_tokens: 6789,
        cost_usd: 0.0123,
      }),
      run({ id: 3, stage: 'auditing', model: 'opus', input_tokens: 200, cost_usd: 0.03 }),
    ];
    render(<StageCostList runs={runs} />);

    expect(screen.getByText('Fact pass')).toBeInTheDocument();
    expect(screen.getByText('12.3k in / 6.8k out')).toBeInTheDocument();
    expect(screen.getByText('$0.0123')).toBeInTheDocument();
    // The mechanical stage is not shown.
    expect(screen.queryByText('Splitting')).not.toBeInTheDocument();
    // Total is the sum.
    expect(screen.getByText('$0.0423')).toBeInTheDocument();
  });

  it('shows a muted note when there is no agent spend', () => {
    render(<StageCostList runs={[run({ stage: 'asr' })]} />);
    expect(screen.getByText(/no agent spend recorded yet/i)).toBeInTheDocument();
  });
});
