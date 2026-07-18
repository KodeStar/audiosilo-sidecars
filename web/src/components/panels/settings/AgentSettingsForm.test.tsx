import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AgentSettingsForm } from './AgentSettingsForm';
import { ApiError } from '@/lib/apiClient';
import type { ApiClient } from '@/lib/apiClient';
import type { AgentConfig, AgentInfo, Settings } from '@/api/types';

const initial: AgentConfig = {
  backend: '',
  concurrency: 2,
  timeout_minutes: 60,
  book_budget_usd: 75,
  claude_models: { fact_pass: 'sonnet' },
  openai_models: {},
};

const info: AgentInfo = { backend: 'claude', available: true, version: '1.0.0' };

function settingsWith(agent: AgentConfig): Settings {
  return {
    listen: '127.0.0.1:8090',
    cors_origins: [],
    secrets: { anthropic_api_key: false, openai_api_key: false, github_pat: false },
    asr: { backend: '' },
    agent,
    contribution: {
      mode: 'issue',
      repo: 'KodeStar/audiosilo-meta',
      auto_purge: true,
      poll_minutes: 10,
    },
    supervisor: {
      enabled: true,
      automatic_actions: false,
      model_assisted: false,
      model_automatic_actions: false,
      allow_backend_failover: false,
    },
  };
}

describe('AgentSettingsForm', () => {
  it('shows the detected runner availability line', () => {
    const client = { updateSettings: vi.fn() } as unknown as ApiClient;
    render(<AgentSettingsForm client={client} initial={initial} info={info} />);
    expect(screen.getByText('Detected runner')).toBeInTheDocument();
    expect(screen.getByText('claude (1.0.0)')).toBeInTheDocument();
  });

  it('saves the whole agent envelope and shows the restart note', async () => {
    const updateSettings = vi.fn().mockResolvedValue(settingsWith(initial));
    const client = { updateSettings } as unknown as ApiClient;
    render(<AgentSettingsForm client={client} initial={initial} info={info} />);

    await userEvent.click(screen.getByRole('button', { name: /save agent settings/i }));

    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    expect(updateSettings).toHaveBeenCalledWith({
      agent: {
        backend: '',
        concurrency: 2,
        timeout_minutes: 60,
        book_budget_usd: 75,
        claude_models: { fact_pass: 'sonnet' },
        openai_models: {},
      },
    });
    expect(await screen.findByText(/restart the daemon to apply/i)).toBeInTheDocument();
  });

  it('surfaces the server 400 message on a rejected save', async () => {
    const client = {
      updateSettings: vi.fn().mockRejectedValue(new ApiError(400, 'agent.backend "x" must be ...')),
    } as unknown as ApiClient;
    render(<AgentSettingsForm client={client} initial={initial} info={info} />);

    await userEvent.click(screen.getByRole('button', { name: /save agent settings/i }));

    expect(await screen.findByText(/agent.backend "x" must be/i)).toBeInTheDocument();
  });

  it('blocks a sub-1 concurrency client-side without calling the API', async () => {
    const updateSettings = vi.fn();
    const client = { updateSettings } as unknown as ApiClient;
    render(<AgentSettingsForm client={client} initial={initial} info={info} />);

    const concurrency = screen.getByLabelText('Concurrency');
    await userEvent.clear(concurrency);
    await userEvent.type(concurrency, '0');
    await userEvent.click(screen.getByRole('button', { name: /save agent settings/i }));

    expect(await screen.findByText(/concurrency must be a whole number/i)).toBeInTheDocument();
    expect(updateSettings).not.toHaveBeenCalled();
  });
});
