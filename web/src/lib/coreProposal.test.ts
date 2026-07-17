import { describe, it, expect } from 'vitest';
import type { BookView, CoreProposal } from '@/api/types';
import {
  coreFormFromBook,
  coreFormToProposal,
  coreProposalToForm,
  emptyCoreForm,
  validateCoreForm,
} from './coreProposal';

function proposal(partial: Partial<CoreProposal>): CoreProposal {
  return {
    title: '',
    subtitle: '',
    authors: [],
    language: '',
    first_published: '',
    series_name: '',
    series_position: '',
    print_isbns: [],
    narrators: [],
    abridged: '',
    runtime_min: 0,
    release_date: '',
    publisher: '',
    asins: [],
    audiobook_isbns: [],
    cover_url: '',
    sources: '',
    ...partial,
  };
}

function bookView(partial: Partial<BookView>): BookView {
  return {
    id: 1,
    source_path: '/books/1',
    title: '',
    authors: [],
    state: 'contributing',
    lane: '',
    status: 'needs_attention',
    progress: [],
    scratch_bytes: 0,
    total_cost_usd: 0,
    created_at: '',
    updated_at: '',
    ...partial,
  };
}

describe('emptyCoreForm', () => {
  it('starts with one blank us-region ASIN row', () => {
    const f = emptyCoreForm();
    expect(f.asins).toEqual([{ region: 'us', asin: '' }]);
    expect(f.abridged).toBe('');
    expect(f.title).toBe('');
  });
});

describe('coreProposalToForm', () => {
  it('joins list fields, stringifies runtime, and preserves ASIN rows', () => {
    const f = coreProposalToForm(
      proposal({
        title: 'The Book',
        authors: ['Jane Roe', 'John Doe'],
        narrators: ['Nora'],
        print_isbns: ['111', '222'],
        runtime_min: 640,
        abridged: 'Unabridged',
        asins: [
          { region: 'us', asin: 'B001' },
          { region: 'uk', asin: 'B002' },
        ],
        sources: 'the audiobook metadata',
      }),
    );
    expect(f.authors).toBe('Jane Roe, John Doe');
    expect(f.narrators).toBe('Nora');
    expect(f.printIsbns).toBe('111, 222');
    expect(f.runtimeMin).toBe('640');
    expect(f.abridged).toBe('Unabridged');
    expect(f.asins).toEqual([
      { region: 'us', asin: 'B001' },
      { region: 'uk', asin: 'B002' },
    ]);
  });

  it('shows one blank us row when the proposal has no ASINs, and 0 runtime as blank', () => {
    const f = coreProposalToForm(proposal({ runtime_min: 0 }));
    expect(f.asins).toEqual([{ region: 'us', asin: '' }]);
    expect(f.runtimeMin).toBe('');
  });
});

describe('coreFormFromBook', () => {
  it('seeds identity fields from the book and its ASIN', () => {
    const f = coreFormFromBook(
      bookView({
        title: 'Book Title',
        authors: ['A One'],
        series: 'Saga',
        series_pos: '3',
        asin: 'B009',
      }),
    );
    expect(f.title).toBe('Book Title');
    expect(f.authors).toBe('A One');
    // BookView carries no narrators, so the fallback form leaves them blank.
    expect(f.narrators).toBe('');
    expect(f.seriesName).toBe('Saga');
    expect(f.seriesPosition).toBe('3');
    expect(f.asins).toEqual([{ region: 'us', asin: 'B009' }]);
  });

  it('leaves a blank ASIN row when the book has no ASIN', () => {
    const f = coreFormFromBook(bookView({ title: 'X' }));
    expect(f.asins).toEqual([{ region: 'us', asin: '' }]);
  });
});

describe('coreFormToProposal', () => {
  it('splits list fields, trims, parses runtime, and drops blank ASIN rows', () => {
    const form = {
      ...emptyCoreForm(),
      title: '  The Book  ',
      authors: 'Jane Roe , John Doe',
      language: ' en ',
      narrators: 'Nora',
      printIsbns: '111,,222',
      runtimeMin: '640',
      abridged: 'Abridged' as const,
      asins: [
        { region: 'us', asin: 'B001' },
        { region: 'uk', asin: '' },
        { region: 'de', asin: ' B003 ' },
      ],
      sources: '  the metadata  ',
    };
    const p = coreFormToProposal(form);
    expect(p.title).toBe('The Book');
    expect(p.authors).toEqual(['Jane Roe', 'John Doe']);
    expect(p.language).toBe('en');
    expect(p.print_isbns).toEqual(['111', '222']);
    expect(p.runtime_min).toBe(640);
    expect(p.abridged).toBe('Abridged');
    expect(p.asins).toEqual([
      { region: 'us', asin: 'B001' },
      { region: 'de', asin: 'B003' },
    ]);
    expect(p.sources).toBe('the metadata');
  });

  it('maps a blank/non-numeric runtime to 0 (unknown), rejecting a lenient prefix', () => {
    // Empty -> omitted (0, the daemon's "unknown").
    expect(coreFormToProposal({ ...emptyCoreForm(), runtimeMin: '' }).runtime_min).toBe(0);
    expect(coreFormToProposal({ ...emptyCoreForm(), runtimeMin: 'abc' }).runtime_min).toBe(0);
    // Strict parsing: "12abc" is NaN -> 0, NOT a lenient prefix parse to 12.
    expect(coreFormToProposal({ ...emptyCoreForm(), runtimeMin: '12abc' }).runtime_min).toBe(0);
    // A clean integer still parses.
    expect(coreFormToProposal({ ...emptyCoreForm(), runtimeMin: ' 90 ' }).runtime_min).toBe(90);
  });

  it('round-trips a proposal through form and back', () => {
    const original = proposal({
      title: 'Round Trip',
      authors: ['A', 'B'],
      language: 'en',
      narrators: ['N'],
      runtime_min: 120,
      asins: [{ region: 'us', asin: 'B1' }],
      audiobook_isbns: ['999'],
      sources: 'src',
      abridged: 'Unabridged',
    });
    expect(coreFormToProposal(coreProposalToForm(original))).toEqual(original);
  });
});

describe('validateCoreForm', () => {
  const good = {
    ...emptyCoreForm(),
    title: 'T',
    authors: 'Jane',
    language: 'en',
    narrators: 'Nora',
    sources: 'the metadata',
  };

  it('passes a complete form', () => {
    expect(validateCoreForm(good)).toBeNull();
  });

  it('rejects each missing required field', () => {
    expect(validateCoreForm({ ...good, title: '  ' })).toMatch(/title/i);
    expect(validateCoreForm({ ...good, authors: '' })).toMatch(/author/i);
    expect(validateCoreForm({ ...good, language: '' })).toMatch(/language/i);
    expect(validateCoreForm({ ...good, narrators: ' , ' })).toMatch(/narrator/i);
    expect(validateCoreForm({ ...good, sources: '' })).toMatch(/source/i);
  });
});
