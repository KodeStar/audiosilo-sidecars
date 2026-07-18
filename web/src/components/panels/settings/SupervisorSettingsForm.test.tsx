import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SupervisorSettingsForm } from './SupervisorSettingsForm';
import type { SupervisorConfig, Settings } from '@/api/types';
import type { ApiClient } from '@/lib/apiClient';

const initial: SupervisorConfig = {
  enabled: true,
  automatic_actions: false,
  model_assisted: false,
  model_automatic_actions: false,
  allow_backend_failover: false,
};

function settings(supervisor: SupervisorConfig): Settings {
  return {
    listen: '127.0.0.1:8090',
    cors_origins: [],
    secrets: { anthropic_api_key: false, openai_api_key: false, github_pat: false },
    asr: { backend: '' },
    agent: {
      backend: '',
      concurrency: 2,
      timeout_minutes: 60,
      book_budget_usd: 75,
      claude_models: {},
      openai_models: {},
    },
    contribution: { mode: 'issue', repo: 'owner/repo', auto_purge: true, poll_minutes: 10 },
    supervisor,
  };
}

describe('SupervisorSettingsForm', () => {
  it('shows conservative defaults and saves each explicit safety gate', async () => {
    const enabled: SupervisorConfig = {
      enabled: true,
      automatic_actions: true,
      model_assisted: true,
      model_automatic_actions: true,
      allow_backend_failover: true,
    };
    const updateSettings = vi.fn().mockResolvedValue(settings(enabled));
    const client = { updateSettings } as unknown as ApiClient;
    render(<SupervisorSettingsForm client={client} initial={initial} />);

    expect(screen.getByLabelText(/deterministic monitoring/i)).toBeChecked();
    expect(screen.getByLabelText(/automatic recovery/i)).not.toBeChecked();
    expect(screen.getByLabelText(/event-driven model diagnosis/i)).not.toBeChecked();
    expect(screen.getByLabelText(/model-recommended actions/i)).not.toBeChecked();
    expect(screen.getByLabelText(/fallback backend/i)).not.toBeChecked();

    await userEvent.click(screen.getByLabelText(/automatic recovery/i));
    await userEvent.click(screen.getByLabelText(/event-driven model diagnosis/i));
    await userEvent.click(screen.getByLabelText(/model-recommended actions/i));
    await userEvent.click(screen.getByLabelText(/fallback backend/i));
    await userEvent.click(screen.getByRole('button', { name: /save supervisor settings/i }));

    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    expect(updateSettings).toHaveBeenCalledWith({ supervisor: enabled });
    expect(await screen.findByText(/restart the daemon to apply/i)).toBeInTheDocument();
  });
});
