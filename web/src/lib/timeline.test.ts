import { describe, it, expect } from 'vitest';
import {
  compactLabel,
  timelineStages,
  COMPACT_LABELS,
  MAINLINE,
  OFF_MAINLINE_AFTER,
  type TimelineStatus,
} from './timeline';
import { LABELS } from './pipelineState';

// Helper: map a stage token to its computed status for terse assertions.
function statusOf(stages: { stage: string; status: TimelineStatus }[], stage: string) {
  return stages.find((s) => s.stage === stage)?.status;
}

describe('timelineStages mainline', () => {
  it('marks strictly-earlier stages done, the current active, and the rest pending', () => {
    const stages = timelineStages('asr', '');
    // asr is the active chip; queued/inspecting/splitting before it are done.
    expect(statusOf(stages, 'queued')).toBe('done');
    expect(statusOf(stages, 'inspecting')).toBe('done');
    expect(statusOf(stages, 'splitting')).toBe('done');
    expect(statusOf(stages, 'asr')).toBe('active');
    expect(statusOf(stages, 'sanitizing')).toBe('pending');
    expect(statusOf(stages, 'done')).toBe('pending');
    // No off-mainline stage is present for a mainline state.
    expect(statusOf(stages, 'markers_normalizing')).toBeUndefined();
    expect(statusOf(stages, 'fixing')).toBeUndefined();
  });

  it('treats queued as active with nothing done', () => {
    const stages = timelineStages('queued', '');
    expect(statusOf(stages, 'queued')).toBe('active');
    expect(stages.filter((s) => s.status === 'done')).toHaveLength(0);
    expect(statusOf(stages, 'inspecting')).toBe('pending');
  });

  it('marks every chip done for a finished book', () => {
    const stages = timelineStages('done', '');
    expect(stages.every((s) => s.status === 'done')).toBe(true);
    expect(statusOf(stages, 'done')).toBe('done');
  });

  it('treats a settled ready waypoint (no status) as idle-complete', () => {
    const stages = timelineStages('ready', '');
    expect(statusOf(stages, 'auditing')).toBe('done');
    // ready reads done (nothing is running), but the M7 contributing stage and
    // done are still pending.
    expect(statusOf(stages, 'ready')).toBe('done');
    expect(statusOf(stages, 'contributing')).toBe('pending');
    expect(statusOf(stages, 'done')).toBe('pending');
  });

  it('keeps ready active when it carries a status', () => {
    const stages = timelineStages('ready', 'paused');
    expect(statusOf(stages, 'ready')).toBe('active');
    expect(statusOf(stages, 'contributing')).toBe('pending');
  });

  it('keeps the current stage active regardless of a park/pause status', () => {
    const stages = timelineStages('fact_pass', 'needs_attention');
    expect(statusOf(stages, 'fact_pass')).toBe('active');
    expect(statusOf(stages, 'synthesizing')).toBe('pending');
    expect(statusOf(stages, 'qa_sweep')).toBe('done');
  });

  it('leaves every chip pending for an unknown state', () => {
    const stages = timelineStages('bogus_stage', '');
    expect(stages.every((s) => s.status === 'pending')).toBe(true);
    expect(statusOf(stages, 'bogus_stage')).toBeUndefined();
  });
});

describe('timelineStages off-mainline insertion', () => {
  it('inserts markers_normalizing after inspecting as the active chip', () => {
    const stages = timelineStages('markers_normalizing', '');
    const order = stages.map((s) => s.stage);
    expect(order.indexOf('markers_normalizing')).toBe(order.indexOf('inspecting') + 1);
    expect(statusOf(stages, 'inspecting')).toBe('done');
    expect(statusOf(stages, 'markers_normalizing')).toBe('active');
    expect(statusOf(stages, 'splitting')).toBe('pending');
  });

  it('inserts qa_adjudicating after qa_sweep as the active chip', () => {
    const stages = timelineStages('qa_adjudicating', '');
    const order = stages.map((s) => s.stage);
    expect(order.indexOf('qa_adjudicating')).toBe(order.indexOf('qa_sweep') + 1);
    expect(statusOf(stages, 'qa_sweep')).toBe('done');
    expect(statusOf(stages, 'qa_adjudicating')).toBe('active');
    expect(statusOf(stages, 'spelling_research')).toBe('pending');
    // The sibling off-mainline stage is not shown.
    expect(statusOf(stages, 'retranscribing')).toBeUndefined();
  });

  it('inserts retranscribing after qa_sweep as the active chip', () => {
    const stages = timelineStages('retranscribing', '');
    const order = stages.map((s) => s.stage);
    expect(order.indexOf('retranscribing')).toBe(order.indexOf('qa_sweep') + 1);
    expect(statusOf(stages, 'qa_sweep')).toBe('done');
    expect(statusOf(stages, 'retranscribing')).toBe('active');
    expect(statusOf(stages, 'spelling_research')).toBe('pending');
    expect(statusOf(stages, 'qa_adjudicating')).toBeUndefined();
  });

  it('inserts fixing after auditing as the active chip', () => {
    const stages = timelineStages('fixing', '');
    const order = stages.map((s) => s.stage);
    expect(order.indexOf('fixing')).toBe(order.indexOf('auditing') + 1);
    expect(statusOf(stages, 'auditing')).toBe('done');
    expect(statusOf(stages, 'fixing')).toBe('active');
    expect(statusOf(stages, 'ready')).toBe('pending');
  });
});

// Drift guard: the timeline module hand-mirrors the pipeline stage graph. Every
// stage it knows about (compact-labelled, on the mainline, or an off-mainline
// insertable) must be a token pipelineState's LABELS map also knows, so a stage
// added or renamed in one map fails here instead of silently drifting.
describe('timeline / pipelineState stage-graph drift guard', () => {
  it('every timeline stage is a known pipelineState label', () => {
    const known = new Set(Object.keys(LABELS));
    const timelineStageTokens = new Set<string>([
      ...Object.keys(COMPACT_LABELS),
      ...MAINLINE,
      ...Object.keys(OFF_MAINLINE_AFTER),
    ]);
    for (const stage of timelineStageTokens) {
      expect(
        known.has(stage),
        `timeline stage "${stage}" is missing from pipelineState LABELS`,
      ).toBe(true);
    }
  });
});

describe('compactLabel', () => {
  it('uses short labels for known stages', () => {
    expect(compactLabel('asr')).toBe('ASR');
    expect(compactLabel('fact_pass')).toBe('Facts');
    expect(compactLabel('qa_sweep')).toBe('QA');
    expect(compactLabel('markers_normalizing')).toBe('Markers');
  });

  it('title-cases an unmapped token', () => {
    expect(compactLabel('some_new_stage')).toBe('Some New Stage');
  });

  it('renders stages omitted from COMPACT_LABELS via the identical fallback', () => {
    // queued/ready/done are intentionally not in COMPACT_LABELS because the
    // title-cased fallback already produces the same string.
    expect(compactLabel('queued')).toBe('Queued');
    expect(compactLabel('ready')).toBe('Ready');
    expect(compactLabel('done')).toBe('Done');
  });
});
