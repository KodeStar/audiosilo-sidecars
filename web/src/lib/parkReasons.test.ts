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
      'contrib_unavailable',
      'core_needed',
      'core_pending',
      'budget_exceeded',
    ];
    for (const code of codes) {
      const hint = parkHint(code);
      expect(hint, code).toBeTruthy();
      expect(hint).toMatch(/\S/);
    }
  });

  it('flips to the auto-retry hint purely on retry_at (the server owns that decision)', () => {
    for (const code of ['agent_unavailable', 'agent_rate_limited']) {
      expect(parkHint(code, true)).toMatch(/automatically/i);
      // Without a scheduled retry, the manual-Retry hint is used.
      expect(parkHint(code, false)).not.toMatch(/automatically/i);
    }
    // No frontend code-list gates this: retry_at drives it for any known code, so the
    // wire field can never drift from a hardcoded set. (The server only sets retry_at on
    // the transient agent parks, but the frontend trusts the field rather than re-deciding.)
    expect(parkHint('budget_exceeded', true)).toMatch(/automatically/i);
    // An unknown/empty code still has no hint, even with retry_at.
    expect(parkHint('bogus_code', true)).toBeNull();
  });

  it('budget_exceeded points at the config lever', () => {
    expect(parkHint('budget_exceeded')).toMatch(/book_budget_usd/);
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

  it('qa_no_converge hint states no fixed round count (it can park after 1 round now)', () => {
    const hint = parkHint('qa_no_converge');
    expect(hint).toBeTruthy();
    expect(hint).not.toMatch(/3 rounds/);
    expect(hint).toMatch(/progress/i);
  });
});
