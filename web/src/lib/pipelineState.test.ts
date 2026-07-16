import { describe, it, expect } from 'vitest';
import { laneOf, stateChipClass, stateLabel, statusBadge } from './pipelineState';

describe('laneOf', () => {
  it('maps stages to their lane (mirroring internal/state)', () => {
    expect(laneOf('asr')).toBe('asr');
    expect(laneOf('retranscribing')).toBe('asr');
    expect(laneOf('fact_pass')).toBe('agent');
    expect(laneOf('auditing')).toBe('agent');
    expect(laneOf('inspecting')).toBe('mechanical');
    expect(laneOf('contributing')).toBe('mechanical');
  });

  it('maps waypoints and unknown states to none', () => {
    expect(laneOf('queued')).toBe('none');
    expect(laneOf('ready')).toBe('none');
    expect(laneOf('done')).toBe('none');
    expect(laneOf('bogus')).toBe('none');
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
  it('gives distinct classes for done and ready waypoints', () => {
    expect(stateChipClass('done')).toContain('success');
    expect(stateChipClass('ready')).toContain('amber');
  });

  it('colors by lane for stages', () => {
    expect(stateChipClass('asr')).toContain('sky');
    expect(stateChipClass('fact_pass')).toContain('pink');
    expect(stateChipClass('inspecting')).toContain('slate');
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
