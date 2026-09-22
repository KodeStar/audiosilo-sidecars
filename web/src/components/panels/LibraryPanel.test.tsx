import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { LibraryPanel } from './LibraryPanel';
import type { ApiClient } from '@/lib/apiClient';
import type { ScanJob, ScannedBook } from '@/api/types';
import { scanStore } from '@/lib/scanStore';

function book(partial: Partial<ScannedBook>): ScannedBook {
  return {
    path: partial.path ?? 'Old Book',
    source_path: '/library/' + (partial.path ?? 'Old Book'),
    title: partial.title ?? 'Old Book',
    audio_files: 1,
    coverage: { available: true, known: false, has_characters: false, has_recaps: false },
    ...partial,
  };
}

// The relative path is deliberately unlike the title: the row renders both, so
// reusing one string would make every getByText ambiguous.
const FRESH = book({ path: 'shelf/fb01', title: 'Fresh Book', is_new: true });
const SEEN = book({ path: 'shelf/sb01', title: 'Seen Book' });

function doneScan(books: ScannedBook[]): ScanJob {
  return {
    id: 'j1',
    path: '/library',
    status: 'done',
    progress: {
      phase: 'done',
      walk_dirs: 1,
      walk_groups: books.length,
      groups_done: books.length,
      groups_total: books.length,
      books_found: books.length,
      coverage_done: books.length,
      coverage_total: books.length,
    },
    books,
  };
}

function makeClient(books: ScannedBook[], over: Partial<Record<keyof ApiClient, unknown>> = {}) {
  return {
    listScans: vi
      .fn()
      .mockResolvedValue({ scans: [{ id: 'j1', path: '/library', status: 'done', progress: {} }] }),
    getScan: vi.fn().mockResolvedValue(doneScan(books)),
    acknowledgeSightings: vi.fn().mockResolvedValue(undefined),
    ...over,
  } as unknown as ApiClient;
}

async function renderPanel(client: ApiClient) {
  render(<LibraryPanel client={client} onProcessed={vi.fn()} />);
  await waitFor(() => expect(screen.getByRole('group', { name: 'Library view' })).toBeTruthy());
}

afterEach(() => {
  scanStore.reset();
  vi.restoreAllMocks();
});

describe('LibraryPanel New/All view', () => {
  it('defaults to New, showing only the newly seen books, and switches to All', async () => {
    await renderPanel(makeClient([FRESH, SEEN]));

    expect(screen.getByRole('button', { name: 'New (1)' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByText('Fresh Book')).toBeInTheDocument();
    expect(screen.queryByText('Seen Book')).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'All (2)' }));

    expect(screen.getByText('Fresh Book')).toBeInTheDocument();
    expect(screen.getByText('Seen Book')).toBeInTheDocument();
    // The "New" pill only appears in the All view, where it distinguishes rows.
    expect(screen.getByText('New')).toBeInTheDocument();
  });

  it('shows the empty-state line instead of an empty table when nothing is new', async () => {
    await renderPanel(makeClient([SEEN]));

    expect(
      screen.getByText(
        'No new books since your last scan. Switch to All to see the whole library.',
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'All (1)' }));
    expect(screen.getByRole('table')).toBeInTheDocument();
  });
});

describe('LibraryPanel dismiss', () => {
  it('acknowledges the selected new books and drops them from the New view', async () => {
    const client = makeClient([FRESH, SEEN]);
    await renderPanel(client);

    const dismiss = screen.getByRole('button', { name: 'Dismiss' });
    expect(dismiss).toBeDisabled();

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select Fresh Book' }));
    expect(dismiss).toBeEnabled();
    await userEvent.click(dismiss);

    await waitFor(() =>
      expect((client.acknowledgeSightings as ReturnType<typeof vi.fn>).mock.calls[0][0]).toEqual([
        '/library/shelf/fb01',
      ]),
    );
    expect(screen.queryByText('Fresh Book')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'New (0)' })).toBeInTheDocument();
    expect(dismiss).toBeDisabled();
  });

  it('surfaces a dismiss failure as a note and keeps the book', async () => {
    const client = makeClient([FRESH, SEEN], {
      acknowledgeSightings: vi.fn().mockRejectedValue(new Error('offline')),
    });
    await renderPanel(client);

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select Fresh Book' }));
    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }));

    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent('Could not dismiss the selected books.'),
    );
    expect(screen.getByText('Fresh Book')).toBeInTheDocument();
  });

  it('leaves the Dismiss button disabled for a selection with nothing new', async () => {
    await renderPanel(makeClient([FRESH, SEEN]));
    await userEvent.click(screen.getByRole('button', { name: 'All (2)' }));

    await userEvent.click(screen.getByRole('checkbox', { name: 'Select Seen Book' }));

    expect(screen.getByRole('button', { name: 'Dismiss' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Process 1 book' })).toBeEnabled();
  });
});
