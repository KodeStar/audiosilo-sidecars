import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { SystemInfo } from '@/api/types';
import type { ApiClient } from '@/lib/apiClient';
import { AppShell } from './AppShell';

vi.mock('@/lib/useEventStream', () => ({
  useEventStream: () => ({ status: 'open', lastHeartbeat: null }),
}));
vi.mock('./panels/LibraryPanel', () => ({ LibraryPanel: () => <div>Library content</div> }));
vi.mock('./panels/RunningPanel', () => ({ RunningPanel: () => <div>Running content</div> }));
vi.mock('./panels/DonePanel', () => ({ DonePanel: () => <div>Done content</div> }));
vi.mock('./panels/SettingsPanel', () => ({ SettingsPanel: () => <div>Settings content</div> }));

const system: SystemInfo = {
  version: 'test',
  data_dir: '/data',
  listen: '127.0.0.1:8090',
  tabs: [
    { id: 'library', label: 'Library', status: 'ready' },
    { id: 'running', label: 'Running', status: 'ready' },
    { id: 'done', label: 'Done', status: 'ready' },
    { id: 'settings', label: 'Settings', status: 'ready' },
  ],
  tools: { ffmpeg: '', ffprobe: '' },
  asr: { backend: '', available: false, device: '', version: '', detail: '' },
  agent: { backend: '', available: false },
  scratch_bytes: 0,
};

function client(): ApiClient {
  return {
    system: vi.fn().mockResolvedValue(system),
    logout: vi.fn().mockResolvedValue(undefined),
  } as unknown as ApiClient;
}

beforeEach(() => {
  window.history.replaceState({}, '', '/');
});

describe('AppShell tab navigation', () => {
  it('opens a deep-linked tab and keeps clicks and popstate in the URL', async () => {
    window.history.replaceState({}, '', '/?tab=running');
    render(<AppShell client={client()} apiBase="" token="token" onSignOut={vi.fn()} />);

    expect(await screen.findByText('Running content')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('tab', { name: 'Done' }));
    expect(await screen.findByText('Done content')).toBeInTheDocument();
    expect(window.location.search).toBe('?tab=done');

    window.history.replaceState({}, '', '/?tab=library');
    fireEvent.popState(window);
    await waitFor(() => expect(screen.getByText('Library content')).toBeInTheDocument());
  });
});
