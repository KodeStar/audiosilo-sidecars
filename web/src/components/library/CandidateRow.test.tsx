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
