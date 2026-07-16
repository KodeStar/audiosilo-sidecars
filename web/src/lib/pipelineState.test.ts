import { describe, it, expect } from 'vitest';
import { normalizeLane, stateChipClass, stateLabel, statusBadge } from './pipelineState';

describe('normalizeLane', () => {
  it('passes through the real lanes', () => {
    expect(normalizeLane('asr')).toBe('asr');
    expect(normalizeLane('agent')).toBe('agent');
    expect(normalizeLane('mechanical')).toBe('mechanical');
  });

  it('maps the empty waypoint lane and anything unknown to none', () => {
    expect(normalizeLane('')).toBe('none');
    expect(normalizeLane('bogus')).toBe('none');
  });
});

describe('stateLabel', () => {
  it('uses friendly labels and title-cases unknowns', () => {
    expect(stateLabel('qa_sweep')).toBe('QA sweep');
    expect(stateLabel('asr')).toBe('Transcribing');
    expect(stateLabel('some_new_stage')).toBe('Some New Stage');
  });
});

describe('stateChipClass', () => {
  it('gives distinct classes for done and ready waypoints (regardless of served lane)', () => {
    expect(stateChipClass('done', '')).toContain('success');
    expect(stateChipClass('ready', '')).toContain('amber');
  });

  it('colors by the served lane for stages', () => {
    expect(stateChipClass('asr', 'asr')).toContain('sky');
    expect(stateChipClass('fact_pass', 'agent')).toContain('pink');
    expect(stateChipClass('inspecting', 'mechanical')).toContain('slate');
  });

  it('falls back to the none style for an empty/unknown lane', () => {
    expect(stateChipClass('inspecting', '')).toContain('raised');
  });
});

describe('statusBadge', () => {
  it('returns null for the normal running condition', () => {
    expect(statusBadge('')).toBeNull();
    expect(statusBadge('none')).toBeNull();
  });

  it('labels exceptional statuses', () => {
    expect(statusBadge('paused')?.label).toBe('Paused');
    expect(statusBadge('needs_attention')?.label).toBe('Needs attention');
    expect(statusBadge('failed')?.label).toBe('Failed');
  });
});
