// Pure form logic for the core (add-work) proposal modal. Kept React-free and
// unit-tested; the modal holds the form state and calls these to seed the form, map
// it to the CoreProposal wire shape, and validate before submit. The daemon
// (contrib.CoreProposal.Validate) is the source of truth - these checks only give
// immediate feedback; a rejected submit still surfaces the server's 400 message.

import type { BookView, CoreProposal, RegionAsin } from '@/api/types';
import { parseIntOrNaN } from '@/lib/formNumbers';

// The Audible marketplaces a core proposal ASIN can be scoped to. `us` is the
// default; the modal offers these in a region select.
export const ASIN_REGIONS = ['us', 'uk', 'ca', 'au', 'de', 'fr', 'es', 'it', 'jp', 'in', 'br'];

export const DEFAULT_ASIN_REGION = 'us';

// The abridged tri-state as it rides the wire: '' = unknown, else the marker.
export type AbridgedValue = '' | 'Unabridged' | 'Abridged';

// One editable ASIN + region pair in the form.
export interface AsinRow {
  region: string;
  asin: string;
}

// CoreProposalForm is the editable model. List fields (authors/narrators/ISBNs) are
// comma-separated strings so the inputs stay plain text; runtimeMin is a raw input
// string so a partially-typed value never coerces to NaN mid-edit. The mapping/
// validation functions parse them.
export interface CoreProposalForm {
  title: string;
  subtitle: string;
  authors: string;
  language: string;
  firstPublished: string;
  seriesName: string;
  seriesPosition: string;
  printIsbns: string;
  narrators: string;
  abridged: AbridgedValue;
  runtimeMin: string;
  releaseDate: string;
  publisher: string;
  asins: AsinRow[];
  audiobookIsbns: string;
  coverUrl: string;
  sources: string;
}

// emptyCoreForm is a blank proposal form with one empty us-region ASIN row.
export function emptyCoreForm(): CoreProposalForm {
  return {
    title: '',
    subtitle: '',
    authors: '',
    language: '',
    firstPublished: '',
    seriesName: '',
    seriesPosition: '',
    printIsbns: '',
    narrators: '',
    abridged: '',
    runtimeMin: '',
    releaseDate: '',
    publisher: '',
    asins: [{ region: DEFAULT_ASIN_REGION, asin: '' }],
    audiobookIsbns: '',
    coverUrl: '',
    sources: '',
  };
}

// splitList parses a comma-separated field into a trimmed, empties-dropped list.
function splitList(raw: string): string[] {
  return raw
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s !== '');
}

// joinList renders a list back into the comma-separated form value.
function joinList(items: string[] | undefined): string {
  return (items ?? []).join(', ');
}

// coreProposalToForm seeds the form from a fetched proposal (the GET prefill). An
// empty ASIN list becomes one blank us row so the modal always shows an editable pair.
export function coreProposalToForm(p: CoreProposal): CoreProposalForm {
  const asins: AsinRow[] =
    p.asins && p.asins.length > 0
      ? p.asins.map((a) => ({ region: a.region || DEFAULT_ASIN_REGION, asin: a.asin }))
      : [{ region: DEFAULT_ASIN_REGION, asin: '' }];
  return {
    title: p.title ?? '',
    subtitle: p.subtitle ?? '',
    authors: joinList(p.authors),
    language: p.language ?? '',
    firstPublished: p.first_published ?? '',
    seriesName: p.series_name ?? '',
    seriesPosition: p.series_position ?? '',
    printIsbns: joinList(p.print_isbns),
    narrators: joinList(p.narrators),
    abridged: p.abridged ?? '',
    runtimeMin: p.runtime_min ? String(p.runtime_min) : '',
    releaseDate: p.release_date ?? '',
    publisher: p.publisher ?? '',
    asins,
    audiobookIsbns: joinList(p.audiobook_isbns),
    coverUrl: p.cover_url ?? '',
    sources: p.sources ?? '',
  };
}

// coreFormFromBook seeds a fresh form from a book's known identity (used when the GET
// prefill 404s). Only the fields a BookView carries are filled; the rest stay blank -
// notably narrators, which the book view does not carry (the daemon's stored proposal
// is the normal prefill source for those).
export function coreFormFromBook(book: BookView): CoreProposalForm {
  const form = emptyCoreForm();
  form.title = book.title ?? '';
  form.authors = joinList(book.authors);
  form.seriesName = book.series ?? '';
  form.seriesPosition = book.series_pos ?? '';
  if (book.asin) form.asins = [{ region: DEFAULT_ASIN_REGION, asin: book.asin }];
  return form;
}

// coreFormToProposal maps the form to the CoreProposal wire shape. Blank ASIN rows
// are dropped; runtimeMin parses to 0 when blank/non-numeric (the daemon treats 0 as
// "unknown") - strict parsing via parseIntOrNaN, so a garbage value like "12abc" is
// NaN (rejected), not a lenient prefix parse. Values are trimmed so trailing
// whitespace never reaches the daemon.
export function coreFormToProposal(form: CoreProposalForm): CoreProposal {
  const asins: RegionAsin[] = form.asins
    .map((a) => ({ region: (a.region || DEFAULT_ASIN_REGION).trim(), asin: a.asin.trim() }))
    .filter((a) => a.asin !== '');
  const runtime = parseIntOrNaN(form.runtimeMin);
  return {
    title: form.title.trim(),
    subtitle: form.subtitle.trim(),
    authors: splitList(form.authors),
    language: form.language.trim(),
    first_published: form.firstPublished.trim(),
    series_name: form.seriesName.trim(),
    series_position: form.seriesPosition.trim(),
    print_isbns: splitList(form.printIsbns),
    narrators: splitList(form.narrators),
    abridged: form.abridged,
    runtime_min: Number.isFinite(runtime) && runtime > 0 ? runtime : 0,
    release_date: form.releaseDate.trim(),
    publisher: form.publisher.trim(),
    asins,
    audiobook_isbns: splitList(form.audiobookIsbns),
    cover_url: form.coverUrl.trim(),
    sources: form.sources.trim(),
  };
}

// validateCoreForm returns a human message for the first client-detectable problem,
// or null when the form looks submittable. Mirrors the daemon's requireds: title,
// >= 1 author, language, >= 1 narrator, non-empty sources. The server re-validates
// and its 400 message wins on any disagreement.
export function validateCoreForm(form: CoreProposalForm): string | null {
  if (form.title.trim() === '') return 'Title is required.';
  if (splitList(form.authors).length === 0) return 'At least one author is required.';
  if (form.language.trim() === '') return 'Language is required (e.g. en).';
  if (splitList(form.narrators).length === 0) return 'At least one narrator is required.';
  if (form.sources.trim() === '') return 'Sources are required (where the facts came from).';
  return null;
}
