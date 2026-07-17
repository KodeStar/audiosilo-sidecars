import { describe, it, expect } from 'vitest';
import { parkHint } from './parkReasons';

describe('parkHint', () => {
  it('maps every known park code to a non-empty actionable hint', () => {
    const codes = [
      'agent_unavailable',
      'agent_rate_limited',
      'agent_validation_exhausted',
      'markers_not_confident',
      'qa_no_converge',
      'spelling_gate_failure',
      'media_tools_unavailable',
      'asr_unavailable',
      'manifest_changed',
      'fix_loop_exhausted',
    ];
    for (const code of codes) {
      const hint = parkHint(code);
      expect(hint, code).toBeTruthy();
      expect(hint).toMatch(/\S/);
    }
  });

  it('returns a specific hint for a representative code', () => {
    expect(parkHint('agent_unavailable')).toBe(
      'Configure an agent backend (Settings > Agent), then Retry.',
    );
  });

  it('returns null for an unknown or empty code', () => {
    expect(parkHint('')).toBeNull();
    expect(parkHint('bogus_code')).toBeNull();
  });
});
