import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { SettingsPanel } from './SettingsPanel';
import type { ApiClient } from '@/lib/apiClient';
import type { AgentInfo, AsrInfo, Settings, SystemInfo, ToolsInfo } from '@/api/types';

const settings: Settings = {
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

const defaultAgent: AgentInfo = { backend: 'claude', available: true, version: '1.0.0' };

const defaultAsr: AsrInfo = {
  backend: 'mlx-whisper',
  available: true,
  device: 'metal',
  version: 'Python 3.12',
  detail: '',
};

function systemWith(tools: ToolsInfo, asr: AsrInfo = defaultAsr): SystemInfo {
  return {
    version: '1.0',
    data_dir: '/data',
    listen: '127.0.0.1:8090',
    tabs: [],
    tools,
    asr,
    agent: defaultAgent,
    scratch_bytes: 0,
  };
}

describe('SettingsPanel media tools', () => {
  it('shows a resolved path and "Not found" for a missing tool', async () => {
    const client = {
      getSettings: vi.fn().mockResolvedValue(settings),
      system: vi.fn().mockResolvedValue(systemWith({ ffmpeg: '/usr/bin/ffmpeg', ffprobe: '' })),
    } as unknown as ApiClient;

    render(<SettingsPanel client={client} />);

    expect(await screen.findByText('Media tools')).toBeInTheDocument();
    expect(await screen.findByText('/usr/bin/ffmpeg')).toBeInTheDocument();
    // ffprobe resolved to empty -> the muted "Not found" placeholder.
    expect(await screen.findByText('Not found')).toBeInTheDocument();
  });

  it('shows the ASR backend and device when available', async () => {
    const client = {
      getSettings: vi.fn().mockResolvedValue(settings),
      system: vi.fn().mockResolvedValue(
        systemWith(
          { ffmpeg: '/usr/bin/ffmpeg', ffprobe: '/usr/bin/ffprobe' },
          {
            backend: 'mlx-whisper',
            available: true,
            device: 'metal',
            version: 'Python 3.12',
            detail: '',
          },
        ),
      ),
    } as unknown as ApiClient;

    render(<SettingsPanel client={client} />);

    expect(await screen.findByText('ASR')).toBeInTheDocument();
    expect(await screen.findByText('mlx-whisper (metal)')).toBeInTheDocument();
  });

  it('shows the muted detail when ASR is unavailable', async () => {
    const client = {
      getSettings: vi.fn().mockResolvedValue(settings),
      system: vi.fn().mockResolvedValue(
        systemWith(
          { ffmpeg: '/usr/bin/ffmpeg', ffprobe: '/usr/bin/ffprobe' },
          {
            backend: 'whisper-cpp',
            available: false,
            device: '',
            version: '',
            detail: 'whisper-cli not found on PATH',
          },
        ),
      ),
    } as unknown as ApiClient;

    render(<SettingsPanel client={client} />);

    expect(await screen.findByText('ASR')).toBeInTheDocument();
    expect(await screen.findByText('whisper-cli not found on PATH')).toBeInTheDocument();
  });
});
