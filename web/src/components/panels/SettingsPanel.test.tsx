import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { SettingsPanel } from './SettingsPanel';
import type { ApiClient } from '@/lib/apiClient';
import type { Settings, SystemInfo, ToolsInfo } from '@/api/types';

const settings: Settings = {
  listen: '127.0.0.1:8090',
  cors_origins: [],
  secrets: { anthropic_api_key: false, openai_api_key: false, github_pat: false },
  asr: { backend: '', device: '' },
  agent: { backend: '', concurrency: 2 },
};

function systemWith(tools: ToolsInfo): SystemInfo {
  return {
    version: '1.0',
    data_dir: '/data',
    listen: '127.0.0.1:8090',
    tabs: [],
    tools,
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
});
