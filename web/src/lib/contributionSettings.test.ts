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
  core_repo: 'KodeStar/audiosilo-meta',
  community_repo: 'KodeStar/audiosilo-meta-community',
  auto_purge: true,
  poll_minutes: 10,
};

function form(partial: Partial<ContributionFormState>): ContributionFormState {
  return {
    mode: 'issue',
    coreRepo: 'Owner/core',
    communityRepo: 'Owner/community',
    autoPurge: true,
    pollMinutes: '10',
    ...partial,
  };
}

describe('contributionConfigToForm', () => {
  it('seeds the form from the config, stringifying poll minutes', () => {
    expect(contributionConfigToForm(config)).toEqual({
      mode: 'issue',
      coreRepo: 'KodeStar/audiosilo-meta',
      communityRepo: 'KodeStar/audiosilo-meta-community',
      autoPurge: true,
      pollMinutes: '10',
    });
  });
});

describe('contributionFormToUpdate', () => {
  it('builds the full envelope, trimming both repos and parsing poll minutes', () => {
    expect(
      contributionFormToUpdate(
        form({
          mode: 'local',
          coreRepo: '  Own/core  ',
          communityRepo: ' Own/comm ',
          autoPurge: false,
          pollMinutes: '30',
        }),
      ),
    ).toEqual({
      mode: 'local',
      core_repo: 'Own/core',
      community_repo: 'Own/comm',
      auto_purge: false,
      poll_minutes: 30,
    });
  });
});

describe('validateContributionForm', () => {
  it('passes a well-formed repo and poll interval', () => {
    expect(validateContributionForm(form({}))).toBeNull();
  });

  it('rejects a core repo that is not owner/name', () => {
    expect(validateContributionForm(form({ coreRepo: 'nowhere' }))).toMatch(
      /core repository.*owner\/name/i,
    );
    expect(validateContributionForm(form({ coreRepo: 'a/b/c' }))).toMatch(/owner\/name/i);
    expect(validateContributionForm(form({ coreRepo: 'a /b' }))).toMatch(/owner\/name/i);
  });

  it('rejects a community repo that is not owner/name', () => {
    expect(validateContributionForm(form({ communityRepo: 'nowhere' }))).toMatch(
      /community repository.*owner\/name/i,
    );
    expect(validateContributionForm(form({ communityRepo: '' }))).toMatch(/community repository/i);
  });

  it('rejects a sub-1 or non-numeric poll interval', () => {
    expect(validateContributionForm(form({ pollMinutes: '0' }))).toMatch(/poll/i);
    expect(validateContributionForm(form({ pollMinutes: '' }))).toMatch(/poll/i);
    expect(validateContributionForm(form({ pollMinutes: 'x' }))).toMatch(/poll/i);
  });
});
