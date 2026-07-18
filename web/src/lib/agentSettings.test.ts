import { describe, it, expect } from 'vitest';
import {
  AGENT_STAGE_KEYS,
  agentConfigToForm,
  agentFormToUpdate,
  validateAgentForm,
  type AgentFormState,
} from './agentSettings';
import type { AgentConfig } from '@/api/types';

const baseConfig: AgentConfig = {
  backend: 'claude',
  concurrency: 2,
  timeout_minutes: 60,
  book_budget_usd: 75,
  claude_models: { fact_pass: 'sonnet', synthesizing: 'opus' },
  openai_models: {},
};

describe('agentConfigToForm', () => {
  it('fills every stage row, defaulting missing cells to empty', () => {
    const form = agentConfigToForm(baseConfig);
    expect(form.backend).toBe('claude');
    expect(form.concurrency).toBe('2');
    expect(form.timeoutMinutes).toBe('60');
    expect(form.bookBudgetUSD).toBe('75');
    // Every agent stage has a row.
    expect(Object.keys(form.models).sort()).toEqual([...AGENT_STAGE_KEYS].sort());
    expect(form.models.fact_pass.claude).toBe('sonnet');
    expect(form.models.synthesizing.claude).toBe('opus');
    // Unset stages / the whole openai column are blank.
    expect(form.models.auditing.claude).toBe('');
    expect(form.models.fact_pass.openai).toBe('');
  });
});

describe('agentFormToUpdate', () => {
  it('round-trips the scalars and collects only non-empty model cells', () => {
    const form = agentConfigToForm(baseConfig);
    form.models.fact_pass.openai = '  gpt-5  '; // trims
    const update = agentFormToUpdate(form);
    expect(update.backend).toBe('claude');
    expect(update.concurrency).toBe(2);
    expect(update.timeout_minutes).toBe(60);
    expect(update.book_budget_usd).toBe(75);
    expect(update.claude_models).toEqual({ fact_pass: 'sonnet', synthesizing: 'opus' });
    expect(update.openai_models).toEqual({ fact_pass: 'gpt-5' });
  });

  it('sends empty maps when a column is entirely blank (backend default)', () => {
    const form = agentConfigToForm({ ...baseConfig, claude_models: {}, openai_models: {} });
    const update = agentFormToUpdate(form);
    expect(update.claude_models).toEqual({});
    expect(update.openai_models).toEqual({});
  });
});

describe('validateAgentForm', () => {
  const ok: AgentFormState = agentConfigToForm(baseConfig);

  it('accepts a valid form', () => {
    expect(validateAgentForm(ok)).toBeNull();
  });

  it('rejects a sub-1 or non-integer concurrency', () => {
    expect(validateAgentForm({ ...ok, concurrency: '0' })).toMatch(/concurrency/i);
    expect(validateAgentForm({ ...ok, concurrency: '' })).toMatch(/concurrency/i);
    expect(validateAgentForm({ ...ok, concurrency: '1.5' })).toMatch(/concurrency/i);
    expect(validateAgentForm({ ...ok, concurrency: 'x' })).toMatch(/concurrency/i);
  });

  it('rejects a sub-1 timeout', () => {
    expect(validateAgentForm({ ...ok, timeoutMinutes: '0' })).toMatch(/timeout/i);
    expect(validateAgentForm({ ...ok, timeoutMinutes: '' })).toMatch(/timeout/i);
  });

  it('rejects a negative or non-numeric book budget but accepts 0 and large values', () => {
    expect(validateAgentForm({ ...ok, bookBudgetUSD: '-1' })).toMatch(/budget/i);
    expect(validateAgentForm({ ...ok, bookBudgetUSD: '' })).toMatch(/budget/i);
    expect(validateAgentForm({ ...ok, bookBudgetUSD: 'x' })).toMatch(/budget/i);
    expect(validateAgentForm({ ...ok, bookBudgetUSD: '0' })).toBeNull();
    expect(validateAgentForm({ ...ok, bookBudgetUSD: '100000' })).toBeNull();
  });
});
