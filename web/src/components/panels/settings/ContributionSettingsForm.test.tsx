import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ContributionSettingsForm } from './ContributionSettingsForm';
import { ApiError } from '@/lib/apiClient';
import type { ApiClient } from '@/lib/apiClient';
import type { AgentConfig, ContributionConfig, Settings } from '@/api/types';

const agent: AgentConfig = {
  backend: '',
  concurrency: 2,
  timeout_minutes: 60,
  book_budget_usd: 75,
  claude_models: {},
  openai_models: {},
};

const initial: ContributionConfig = {
  mode: 'issue',
  repo: 'KodeStar/audiosilo-meta',
  auto_purge: true,
  poll_minutes: 10,
};

function settingsWith(contribution: ContributionConfig): Settings {
  return {
    listen: '127.0.0.1:8090',
    cors_origins: [],
    secrets: { anthropic_api_key: false, openai_api_key: false, github_pat: false },
    asr: { backend: '' },
    agent,
    contribution,
    supervisor: {
      enabled: true,
      automatic_actions: false,
      model_assisted: false,
      model_automatic_actions: false,
      allow_backend_failover: false,
    },
  };
}

describe('ContributionSettingsForm', () => {
  it('renders the current contribution settings', () => {
    const client = { updateSettings: vi.fn() } as unknown as ApiClient;
    render(<ContributionSettingsForm client={client} initial={initial} />);
    expect(screen.getByLabelText('Repository (owner/name)')).toHaveValue('KodeStar/audiosilo-meta');
    expect(screen.getByLabelText('Poll interval (minutes)')).toHaveValue(10);
    expect(screen.getByLabelText('Auto-purge scratch when a book reaches done')).toBeChecked();
  });

  it('saves the whole contribution envelope with edits and shows the restart note', async () => {
    const updateSettings = vi.fn().mockResolvedValue(settingsWith(initial));
    const client = { updateSettings } as unknown as ApiClient;
    render(<ContributionSettingsForm client={client} initial={initial} />);

    const repo = screen.getByLabelText('Repository (owner/name)');
    await userEvent.clear(repo);
    await userEvent.type(repo, 'someone/other-repo');

    const poll = screen.getByLabelText('Poll interval (minutes)');
    await userEvent.clear(poll);
    await userEvent.type(poll, '15');

    await userEvent.click(screen.getByRole('button', { name: /save contribution settings/i }));

    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    expect(updateSettings).toHaveBeenCalledWith({
      contribution: {
        mode: 'issue',
        repo: 'someone/other-repo',
        auto_purge: true,
        poll_minutes: 15,
      },
    });
    expect(await screen.findByText(/restart the daemon to apply/i)).toBeInTheDocument();
  });

  it('surfaces the server 400 message on a rejected save', async () => {
    const client = {
      updateSettings: vi.fn().mockRejectedValue(new ApiError(400, 'contribution.repo is invalid')),
    } as unknown as ApiClient;
    render(<ContributionSettingsForm client={client} initial={initial} />);

    await userEvent.click(screen.getByRole('button', { name: /save contribution settings/i }));

    expect(await screen.findByText(/contribution.repo is invalid/i)).toBeInTheDocument();
  });

  it('blocks a sub-1 poll interval client-side without calling the API', async () => {
    const updateSettings = vi.fn();
    const client = { updateSettings } as unknown as ApiClient;
    render(<ContributionSettingsForm client={client} initial={initial} />);

    const poll = screen.getByLabelText('Poll interval (minutes)');
    await userEvent.clear(poll);
    await userEvent.type(poll, '0');
    await userEvent.click(screen.getByRole('button', { name: /save contribution settings/i }));

    expect(await screen.findByText(/poll interval must be a whole number/i)).toBeInTheDocument();
    expect(updateSettings).not.toHaveBeenCalled();
  });
});
