import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { RunningPanel } from './RunningPanel';
import type { ApiClient } from '@/lib/apiClient';
import type { SystemInfo } from '@/api/types';

// jsdom has no EventSource; the panel opens one via useEventStream. Stub a no-op.
class FakeEventSource {
  static readonly CLOSED = 2;
  readyState = 0;
  close() {}
  addEventListener() {}
  removeEventListener() {}
}

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
});
afterEach(() => {
  vi.unstubAllGlobals();
});

function system(scratchBytes: number): SystemInfo {
  return {
    version: '1.0',
    data_dir: '/data',
    listen: '127.0.0.1:8090',
    tabs: [],
    tools: { ffmpeg: '/usr/bin/ffmpeg', ffprobe: '/usr/bin/ffprobe' },
    asr: {
      backend: 'mlx-whisper',
      available: true,
      device: 'metal',
      version: 'Python 3.12',
      detail: '',
    },
    scratch_bytes: scratchBytes,
  };
}

describe('RunningPanel scratch gauge', () => {
  it('renders the daemon-total scratch from /system in the header strip', async () => {
    const client = {
      listBooks: vi.fn().mockResolvedValue({ books: [] }),
      system: vi.fn().mockResolvedValue(system(1536)),
    } as unknown as ApiClient;

    render(<RunningPanel client={client} apiBase="" token="tok" />);

    // formatBytes(1536) === "1.5 KB", labelled Scratch.
    expect(await screen.findByText('1.5 KB')).toBeInTheDocument();
    expect(screen.getByText('Scratch')).toBeInTheDocument();
  });
});
