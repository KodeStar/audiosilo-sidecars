// Pure view<->update mapping for the Settings "Contribution" card. Kept React-free
// and unit-tested; the form component holds the state and calls these to derive the
// PUT /settings envelope and a client-side validation hint. The daemon
// (config.Validate) is the source of truth for validation - these hints only give
// immediate feedback; a rejected save still surfaces the server's 400 message.

import type { ContributionConfig, ContributionUpdate } from '@/api/types';
import { parseIntOrNaN } from '@/lib/formNumbers';

// The three publish modes for the contributing stage.
export const CONTRIBUTION_MODES: { value: string; label: string }[] = [
  { value: 'issue', label: 'Issue (intake bot composes the PR)' },
  { value: 'pr', label: 'Pull request (direct)' },
  { value: 'local', label: 'Local export only' },
];

// ContributionFormState is the editable form model. pollMinutes is a raw input
// string so a partially-typed value never coerces to NaN mid-edit; the mapping/
// validation functions parse it.
export interface ContributionFormState {
  mode: string;
  repo: string;
  autoPurge: boolean;
  pollMinutes: string;
}

// contributionConfigToForm seeds the form from the loaded settings.
export function contributionConfigToForm(cfg: ContributionConfig): ContributionFormState {
  return {
    mode: cfg.mode,
    repo: cfg.repo,
    autoPurge: cfg.auto_purge,
    pollMinutes: String(cfg.poll_minutes),
  };
}

// contributionFormToUpdate builds the full contribution envelope for PUT /settings
// (the card saves the whole block at once).
export function contributionFormToUpdate(form: ContributionFormState): ContributionUpdate {
  return {
    mode: form.mode,
    repo: form.repo.trim(),
    auto_purge: form.autoPurge,
    poll_minutes: parseIntOrNaN(form.pollMinutes),
  };
}

// validateContributionForm returns a human message for the first client-detectable
// problem, or null when the form looks savable. The server re-validates (repo
// owner/name shape, mode enum, poll interval) and its 400 message wins on any
// disagreement.
export function validateContributionForm(form: ContributionFormState): string | null {
  const repo = form.repo.trim();
  // owner/name: a single slash, no spaces, non-empty halves.
  if (!/^[^/\s]+\/[^/\s]+$/.test(repo)) {
    return 'Repository must be in owner/name form (e.g. KodeStar/audiosilo-meta).';
  }
  const p = parseIntOrNaN(form.pollMinutes);
  if (!Number.isInteger(p) || p < 1) {
    return 'Poll interval must be a whole number of at least 1 minute.';
  }
  return null;
}
