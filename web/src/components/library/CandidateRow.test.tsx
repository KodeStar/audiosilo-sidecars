import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ScannedBook } from '@/api/types';
import { CandidateRow } from './CandidateRow';

function book(partial: Partial<ScannedBook> = {}): ScannedBook {
  return {
    path: 'Unintended Cultivator/UC01',
    source_path: '/library/Unintended Cultivator/UC01',
    title: 'Unintended Cultivator, Volume One',
    audio_files: 1,
    coverage: {
      available: true,
      known: false,
      has_characters: false,
      has_recaps: false,
    },
    ...partial,
  };
}

function renderRow(value: ScannedBook) {
  return render(
    <table>
      <tbody>
        <CandidateRow
          book={value}
          checked={false}
          onToggle={vi.fn()}
          onMatch={vi.fn()}
          onHide={vi.fn()}
        />
      </tbody>
    </table>,
  );
}

describe('CandidateRow pipeline presence', () => {
  it('keeps a completed book visible but removes its selection and match controls', () => {
    renderRow(book({ pipeline_book: { id: 9, state: 'done', status: '' } }));

    expect(screen.getByText('Unintended Cultivator, Volume One')).toBeInTheDocument();
    expect(screen.getByText('Completed')).toBeInTheDocument();
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Match' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Hide' })).toBeInTheDocument();
  });

  it('labels an active pipeline book as in queue and removes its checkbox', () => {
    renderRow(book({ pipeline_book: { id: 10, state: 'asr', status: '' } }));

    expect(screen.getByText('In queue')).toHaveAttribute(
      'title',
      'Pipeline book #10: Transcribing',
    );
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
  });

  it('leaves an untracked candidate selectable', () => {
    renderRow(book());

    expect(
      screen.getByRole('checkbox', { name: 'Select Unintended Cultivator, Volume One' }),
    ).toBeInTheDocument();
  });
});

describe('epub candidates', () => {
  const base = {
    path: 'a',
    source_path: '/lib/a',
    title: 'A Book',
    audio_files: 1,
    coverage: { available: false } as ScannedBook['coverage'],
  } as ScannedBook;

  function renderRow(book: ScannedBook, onToggleSource?: (b: ScannedBook, f: boolean) => void) {
    return render(
      <table>
        <tbody>
          <CandidateRow
            book={book}
            checked={false}
            onToggle={() => {}}
            onToggleSource={onToggleSource}
          />
        </tbody>
      </table>,
    );
  }

  it('badges an ebook-only candidate and offers no audio fallback', () => {
    renderRow({ ...base, kind: 'ebook', ebook_path: '/lib/a', source_path: '/lib/a' }, () => {});
    expect(screen.getByText('EPUB')).toBeTruthy();
    // There is no audio for an ebook-only book, so the toggle must not appear -
    // using it would enqueue an .epub into ffprobe.
    expect(screen.queryByText('Use audio')).toBeNull();
  });

  it('badges a hybrid candidate and offers the audio fallback', () => {
    const onToggle = vi.fn();
    renderRow({ ...base, kind: 'ebook', ebook_path: '/lib/a/book.epub' }, onToggle);
    expect(screen.getByText('EPUB + audio')).toBeTruthy();
    screen.getByText('Use audio').click();
    expect(onToggle).toHaveBeenCalledWith(expect.objectContaining({ source_path: '/lib/a' }), true);
  });

  it('offers the epub back once audio is forced', () => {
    const onToggle = vi.fn();
    // force_audio applied: the server states it; kind is cleared because that is
    // what the pipeline acts on, but the flag is what the toggle renders from.
    renderRow({ ...base, ebook_path: '/lib/a/book.epub', force_audio: true }, onToggle);
    screen.getByText('Use epub').click();
    expect(onToggle).toHaveBeenCalledWith(expect.anything(), false);
  });

  it('shows the note explaining a skipped epub', () => {
    renderRow({ ...base, ebook_note: '3 epubs in this folder - none selected' });
    expect(screen.getByText(/3 epubs in this folder/)).toBeTruthy();
  });
});
