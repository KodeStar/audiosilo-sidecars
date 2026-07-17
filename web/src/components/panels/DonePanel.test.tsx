import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DonePanel } from './DonePanel';
import { ApiClient, ApiError } from '@/lib/apiClient';
import type { BookDetail, BookView, SidecarsView } from '@/api/types';

function book(partial: Partial<BookView>): BookView {
  return {
    id: 1,
    source_path: '/books/1',
    title: 'A Finished Book',
    authors: ['An Author'],
    state: 'done',
    lane: '',
    status: '',
    progress: [],
    scratch_bytes: 0,
    total_cost_usd: 0.1234,
    created_at: '2026-07-17T00:00:00.000000000Z',
    updated_at: '2026-07-17T00:00:00.000000000Z',
    ...partial,
  };
}

function detail(partial: Partial<BookDetail>): BookDetail {
  return {
    ...book({}),
    stage_runs: [],
    ...partial,
  };
}

const SIDECARS: SidecarsView = {
  work: 'a-finished-book',
  characters: [
    {
      id: 'jane-doe',
      name: 'Jane Doe',
      aliases: ['JD'],
      role: 'protagonist',
      reveal: { chapter: 1 },
      description: 'The secret backstory of Jane.',
    },
  ],
  recaps: [{ through: { chapter: 3 }, scope: 'book', text: 'A lot happened by chapter three.' }],
};

function makeClient(over: Partial<Record<keyof ApiClient, unknown>>): ApiClient {
  return {
    listBooks: vi.fn().mockResolvedValue({ books: [] }),
    getBook: vi.fn().mockResolvedValue(detail({})),
    getBookSidecars: vi.fn().mockResolvedValue(SIDECARS),
    purgeScratch: vi.fn().mockResolvedValue(undefined),
    deleteBook: vi.fn().mockResolvedValue(undefined),
    ...over,
  } as unknown as ApiClient;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('DonePanel', () => {
  it('renders done books and filters non-done books out', async () => {
    const client = makeClient({
      listBooks: vi.fn().mockResolvedValue({
        books: [
          book({ id: 1, title: 'Done One', state: 'done' }),
          book({ id: 2, title: 'Still Running', state: 'auditing' }),
        ],
      }),
    });
    render(<DonePanel client={client} />);

    expect(await screen.findByText('Done One')).toBeInTheDocument();
    expect(screen.queryByText('Still Running')).not.toBeInTheDocument();
  });

  it('shows an empty-state card when there are no done books', async () => {
    const client = makeClient({
      listBooks: vi.fn().mockResolvedValue({ books: [book({ state: 'asr' })] }),
    });
    render(<DonePanel client={client} />);

    expect(await screen.findByText(/No finished books yet/i)).toBeInTheDocument();
  });

  it('renders the per-stage cost table from getBook when Details is opened', async () => {
    const client = makeClient({
      listBooks: vi.fn().mockResolvedValue({ books: [book({ id: 7 })] }),
      getBook: vi.fn().mockResolvedValue(
        detail({
          id: 7,
          stage_runs: [
            {
              id: 1,
              book_id: 7,
              stage: 'fact_pass',
              attempt: 1,
              started_at: '2026-07-17T00:00:00.000000000Z',
              finished_at: '2026-07-17T00:04:00.000000000Z',
              ok: true,
              model: 'claude-opus',
              input_tokens: 1200,
              output_tokens: 800,
              cost_usd: 0.05,
            },
          ],
        }),
      ),
    });
    render(<DonePanel client={client} />);

    const detailsBtn = await screen.findByRole('button', { name: 'Details' });
    await userEvent.click(detailsBtn);

    expect(await screen.findByText('Fact pass')).toBeInTheDocument();
    expect(screen.getByText('claude-opus')).toBeInTheDocument();
    // Elapsed 4m, cost formatted.
    expect(screen.getByText('4m')).toBeInTheDocument();
    expect(screen.getAllByText('$0.0500').length).toBeGreaterThan(0);
  });

  it('previews characters + recaps with the description hidden until clicked', async () => {
    const client = makeClient({
      listBooks: vi.fn().mockResolvedValue({ books: [book({ id: 3 })] }),
    });
    render(<DonePanel client={client} />);

    const previewBtn = await screen.findByRole('button', { name: 'Preview' });
    await userEvent.click(previewBtn);

    // Character name + role visible; description behind a closed accordion.
    expect(await screen.findByText('Jane Doe')).toBeInTheDocument();
    expect(screen.getByText('Protagonist')).toBeInTheDocument();
    expect(screen.queryByText('The secret backstory of Jane.')).not.toBeInTheDocument();

    // Opening the reveal accordion (aria-expanded=false) shows it.
    const revealToggle = screen.getByRole('button', { name: /From the start/i });
    expect(revealToggle).toHaveAttribute('aria-expanded', 'false');
    await userEvent.click(revealToggle);
    expect(screen.getByText('The secret backstory of Jane.')).toBeInTheDocument();

    // The Story so far tab is present (recaps exist).
    expect(screen.getByRole('tab', { name: /Story so far/i })).toBeInTheDocument();
  });

  it('shows a quiet note when the sidecars endpoint 404s', async () => {
    const client = makeClient({
      listBooks: vi.fn().mockResolvedValue({ books: [book({ id: 4 })] }),
      getBookSidecars: vi.fn().mockRejectedValue(new ApiError(404, 'not found')),
    });
    render(<DonePanel client={client} />);

    await userEvent.click(await screen.findByRole('button', { name: 'Preview' }));
    expect(await screen.findByText(/No sidecars produced/i)).toBeInTheDocument();
  });

  it('purges and deletes after a confirm', async () => {
    const purgeScratch = vi.fn().mockResolvedValue(undefined);
    const deleteBook = vi.fn().mockResolvedValue(undefined);
    const client = makeClient({
      listBooks: vi.fn().mockResolvedValue({ books: [book({ id: 9, scratch_bytes: 2048 })] }),
      purgeScratch,
      deleteBook,
    });
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    render(<DonePanel client={client} />);

    await userEvent.click(await screen.findByRole('button', { name: 'Free disk' }));
    await waitFor(() => expect(purgeScratch).toHaveBeenCalledWith(9));

    await userEvent.click(screen.getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(deleteBook).toHaveBeenCalledWith(9));
  });

  it('does not act when the confirm is dismissed', async () => {
    const deleteBook = vi.fn().mockResolvedValue(undefined);
    const client = makeClient({
      listBooks: vi.fn().mockResolvedValue({ books: [book({ id: 5 })] }),
      deleteBook,
    });
    vi.spyOn(window, 'confirm').mockReturnValue(false);
    render(<DonePanel client={client} />);

    await userEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    expect(deleteBook).not.toHaveBeenCalled();
  });
});
