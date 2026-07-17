import { describe, it, expect } from 'vitest';
import type { ContributionConfig } from '@/api/types';
import {
  contributionConfigToForm,
  contributionFormToUpdate,
  validateContributionForm,
  type ContributionFormState,
} from './contributionSettings';

const config: ContributionConfig = {
  mode: 'issue',
  repo: 'KodeStar/audiosilo-meta',
  auto_purge: true,
  poll_minutes: 10,
};

function form(partial: Partial<ContributionFormState>): ContributionFormState {
  return { mode: 'issue', repo: 'Owner/name', autoPurge: true, pollMinutes: '10', ...partial };
}

describe('contributionConfigToForm', () => {
  it('seeds the form from the config, stringifying poll minutes', () => {
    expect(contributionConfigToForm(config)).toEqual({
      mode: 'issue',
      repo: 'KodeStar/audiosilo-meta',
      autoPurge: true,
      pollMinutes: '10',
    });
  });
});

describe('contributionFormToUpdate', () => {
  it('builds the full envelope, trimming repo and parsing poll minutes', () => {
    expect(
      contributionFormToUpdate(
        form({ mode: 'pr', repo: '  Own/nm  ', autoPurge: false, pollMinutes: '30' }),
      ),
    ).toEqual({ mode: 'pr', repo: 'Own/nm', auto_purge: false, poll_minutes: 30 });
  });
});

describe('validateContributionForm', () => {
  it('passes a well-formed repo and poll interval', () => {
    expect(validateContributionForm(form({}))).toBeNull();
  });

  it('rejects a repo that is not owner/name', () => {
    expect(validateContributionForm(form({ repo: 'nowhere' }))).toMatch(/owner\/name/i);
    expect(validateContributionForm(form({ repo: 'a/b/c' }))).toMatch(/owner\/name/i);
    expect(validateContributionForm(form({ repo: 'a /b' }))).toMatch(/owner\/name/i);
  });

  it('rejects a sub-1 or non-numeric poll interval', () => {
    expect(validateContributionForm(form({ pollMinutes: '0' }))).toMatch(/poll/i);
    expect(validateContributionForm(form({ pollMinutes: '' }))).toMatch(/poll/i);
    expect(validateContributionForm(form({ pollMinutes: 'x' }))).toMatch(/poll/i);
  });
});
