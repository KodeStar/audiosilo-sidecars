// Pure view<->update mapping for the Settings "Agent" card. Kept React-free and
// unit-tested; the form component holds the state and calls these to derive the
// PUT /settings envelope and a client-side validation hint. The daemon
// (config.Validate) is the source of truth for validation - these hints only give
// immediate feedback; a rejected save still surfaces the server's 400 message.

import type { AgentConfig, AgentUpdate } from '@/api/types';
import { parseFloatOrNaN, parseIntOrNaN } from '@/lib/formNumbers';

// The agent-lane stage names, in pipeline order. Mirrors the Go config's
// defaultClaudeModels keys / state.IsAgent set - a model-map key outside this set
// is rejected by the daemon.
export const AGENT_STAGE_KEYS = [
  'markers_normalizing',
  'qa_adjudicating',
  'spelling_research',
  'fact_pass',
  'synthesizing',
  'auditing',
  'fixing',
] as const;

export type AgentStageKey = (typeof AGENT_STAGE_KEYS)[number];

// AgentFormState is the editable form model. Concurrency/timeout are kept as raw
// input strings so a partially-typed value never coerces to NaN mid-edit; the
// mapping/validation functions parse them. models holds one row per stage with the
// two editable model cells (empty = the backend default).
export interface AgentFormState {
  backend: string; // '' (auto) | 'claude' | 'codex'
  concurrency: string;
  timeoutMinutes: string;
  bookBudgetUSD: string; // per-book agent spend cap, USD (a large value effectively disables)
  models: Record<AgentStageKey, { claude: string; openai: string }>;
}

// agentConfigToForm seeds the form from the loaded settings, filling every stage
// row (a stage absent from the config map shows an empty cell = the default).
export function agentConfigToForm(agent: AgentConfig): AgentFormState {
  const models = {} as AgentFormState['models'];
  for (const key of AGENT_STAGE_KEYS) {
    models[key] = {
      claude: agent.claude_models[key] ?? '',
      openai: agent.openai_models[key] ?? '',
    };
  }
  return {
    backend: agent.backend,
    concurrency: String(agent.concurrency),
    timeoutMinutes: String(agent.timeout_minutes),
    bookBudgetUSD: String(agent.book_budget_usd),
    models,
  };
}

// nonEmptyModels collects the non-blank cells of one column into a wire map
// (stage -> model), trimming values. An empty column yields {} - the daemon
// replaces the config map wholesale, so an omitted stage means "backend default".
function nonEmptyModels(form: AgentFormState, col: 'claude' | 'openai'): Record<string, string> {
  const out: Record<string, string> = {};
  for (const key of AGENT_STAGE_KEYS) {
    const v = form.models[key][col].trim();
    if (v !== '') out[key] = v;
  }
  return out;
}

// agentFormToUpdate builds the full agent envelope for PUT /settings. Every field
// is sent (the card saves the whole agent block at once); the model maps replace
// the config maps wholesale.
export function agentFormToUpdate(form: AgentFormState): AgentUpdate {
  return {
    backend: form.backend,
    concurrency: parseIntOrNaN(form.concurrency),
    timeout_minutes: parseIntOrNaN(form.timeoutMinutes),
    book_budget_usd: parseFloatOrNaN(form.bookBudgetUSD),
    claude_models: nonEmptyModels(form, 'claude'),
    openai_models: nonEmptyModels(form, 'openai'),
  };
}

// validateAgentForm returns a human message for the first client-detectable
// problem, or null when the form looks savable. The server re-validates and its
// 400 message wins on any disagreement.
export function validateAgentForm(form: AgentFormState): string | null {
  const c = parseIntOrNaN(form.concurrency);
  if (!Number.isInteger(c) || c < 1) {
    return 'Concurrency must be a whole number of at least 1.';
  }
  const t = parseIntOrNaN(form.timeoutMinutes);
  if (!Number.isInteger(t) || t < 1) {
    return 'Timeout must be a whole number of at least 1 minute.';
  }
  const budget = parseFloatOrNaN(form.bookBudgetUSD);
  if (!Number.isFinite(budget) || budget < 0) {
    return 'Book budget must be a non-negative dollar amount (set a large value to effectively disable it).';
  }
  return null;
}

// The three selectable backends for the card's dropdown, label included.
export const AGENT_BACKENDS: { value: string; label: string }[] = [
  { value: '', label: 'Auto (default)' },
  { value: 'claude', label: 'Claude' },
  { value: 'codex', label: 'Codex' },
];
