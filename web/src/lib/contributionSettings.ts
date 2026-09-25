// Pure view<->update mapping for the Settings "Contribution" card. Kept React-free
// and unit-tested; the form component holds the state and calls these to derive the
// PUT /settings envelope and a client-side validation hint. The daemon
// (config.Validate) is the source of truth for validation - these hints only give
// immediate feedback; a rejected save still surfaces the server's 400 message.

import type { ContributionConfig, ContributionUpdate } from '@/api/types';
import { parseIntOrNaN } from '@/lib/formNumbers';

// The publish modes for the contributing stage (the direct-PR mode is retired).
export const CONTRIBUTION_MODES: { value: string; label: string }[] = [
  { value: 'issue', label: 'Issue (intake bot composes the PR)' },
  { value: 'local', label: 'Local export only (one sidecar file per kind)' },
];

// ContributionFormState is the editable form model. pollMinutes is a raw input
// string so a partially-typed value never coerces to NaN mid-edit; the mapping/
// validation functions parse it.
export interface ContributionFormState {
  mode: string;
  coreRepo: string;
  communityRepo: string;
  autoPurge: boolean;
  pollMinutes: string;
}

// REPO_FIELDS are the two halves of the split metadata database, rendered and
// validated from this one list.
export const REPO_FIELDS: {
  key: 'communityRepo' | 'coreRepo';
  id: string;
  label: string;
  example: string;
  hint: string;
}[] = [
  {
    key: 'communityRepo',
    id: 'contrib-community-repo',
    label: 'Community repository',
    example: 'KodeStar/audiosilo-meta-community',
    hint: 'Receives the characters and recaps sidecars (the CC BY-SA layer).',
  },
  {
    key: 'coreRepo',
    id: 'contrib-core-repo',
    label: 'Core repository',
    example: 'KodeStar/audiosilo-meta',
    hint: 'Receives add-work proposals, for a book whose work is not on AudioSilo Meta yet.',
  },
];

// contributionConfigToForm seeds the form from the loaded settings.
export function contributionConfigToForm(cfg: ContributionConfig): ContributionFormState {
  return {
    mode: cfg.mode,
    coreRepo: cfg.core_repo,
    communityRepo: cfg.community_repo,
    autoPurge: cfg.auto_purge,
    pollMinutes: String(cfg.poll_minutes),
  };
}

// contributionFormToUpdate builds the full contribution envelope for PUT /settings
// (the card saves the whole block at once).
export function contributionFormToUpdate(form: ContributionFormState): ContributionUpdate {
  return {
    mode: form.mode,
    core_repo: form.coreRepo.trim(),
    community_repo: form.communityRepo.trim(),
    auto_purge: form.autoPurge,
    poll_minutes: parseIntOrNaN(form.pollMinutes),
  };
}

// validateContributionForm returns a human message for the first client-detectable
// problem, or null when the form looks savable. The server re-validates (repo
// owner/name shape, mode enum, poll interval) and its 400 message wins on any
// disagreement.
export function validateContributionForm(form: ContributionFormState): string | null {
  // owner/name: a single slash, no spaces, non-empty halves.
  for (const f of REPO_FIELDS) {
    if (!/^[^/\s]+\/[^/\s]+$/.test(form[f.key].trim())) {
      return `${f.label} must be in owner/name form (e.g. ${f.example}).`;
    }
  }
  const p = parseIntOrNaN(form.pollMinutes);
  if (!Number.isInteger(p) || p < 1) {
    return 'Poll interval must be a whole number of at least 1 minute.';
  }
  return null;
}
