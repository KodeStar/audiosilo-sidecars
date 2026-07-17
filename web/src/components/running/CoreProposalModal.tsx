import { useEffect, useState } from 'react';
import type { BookView } from '@/api/types';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import { Modal } from '@/components/ui/Modal';
import { Field } from '@/components/ui/Field';
import {
  ASIN_REGIONS,
  DEFAULT_ASIN_REGION,
  coreFormFromBook,
  coreFormToProposal,
  coreProposalToForm,
  emptyCoreForm,
  validateCoreForm,
  type AbridgedValue,
  type CoreProposalForm,
} from '@/lib/coreProposal';

interface CoreProposalModalProps {
  book: BookView;
  client: ApiClient;
  onClose: () => void;
  // Called after a successful submit (core proposal OR attach-existing-work). The
  // panel closes the modal and refetches so the park flips (core_needed ->
  // core_pending, or the book re-admits).
  onDone: () => void;
}

// CoreProposalModal completes the add-work metadata proposal for a book whose work
// is not yet on AudioSilo Meta (parked core_needed). It prefills from the daemon's
// stored proposal (GET .../contrib/core), falling back to the book's known identity
// on a 404, and submits via POST .../contribute/core. A secondary path attaches an
// existing work slug (POST .../work) when the work in fact already exists upstream.
export function CoreProposalModal({ book, client, onClose, onDone }: CoreProposalModalProps) {
  const [form, setForm] = useState<CoreProposalForm | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [workSlug, setWorkSlug] = useState(book.work_id ?? '');

  // Prefill: the daemon writes contrib/core_proposal.json when it parks the book, so
  // the GET usually returns it. A 404 (or any read error) falls back to the book's
  // own identity fields.
  useEffect(() => {
    let cancelled = false;
    client
      .getCoreProposal(book.id)
      .then((p) => {
        if (!cancelled) setForm(coreProposalToForm(p));
      })
      .catch(() => {
        if (!cancelled) setForm(coreFormFromBook(book));
      });
    return () => {
      cancelled = true;
    };
    // book is stable for the modal's lifetime; only the id/client matter.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, book.id]);

  function update<K extends keyof CoreProposalForm>(key: K, value: CoreProposalForm[K]) {
    setForm((f) => (f ? { ...f, [key]: value } : f));
  }

  function setAsin(index: number, field: 'region' | 'asin', value: string) {
    setForm((f) => {
      if (!f) return f;
      const asins = f.asins.map((row, i) => (i === index ? { ...row, [field]: value } : row));
      return { ...f, asins };
    });
  }

  function addAsinRow() {
    setForm((f) =>
      f ? { ...f, asins: [...f.asins, { region: DEFAULT_ASIN_REGION, asin: '' }] } : f,
    );
  }

  function removeAsinRow(index: number) {
    setForm((f) => {
      if (!f) return f;
      const asins = f.asins.filter((_, i) => i !== index);
      return { ...f, asins: asins.length > 0 ? asins : emptyCoreForm().asins };
    });
  }

  async function submitCore() {
    if (!form) return;
    const hint = validateCoreForm(form);
    if (hint) {
      setError(hint);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await client.submitCoreProposal(book.id, coreFormToProposal(form));
      onDone();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not submit the proposal.');
    } finally {
      setBusy(false);
    }
  }

  async function attachExisting() {
    const slug = workSlug.trim();
    if (slug === '') {
      setError('Enter the work slug to attach.');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await client.setBookWork(book.id, slug);
      onDone();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not attach the work.');
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      ariaLabel={`Work proposal for ${book.title}`}
      onClose={onClose}
      maxWidthClass="max-w-2xl"
      title="Work proposal"
      subtitle={book.title}
      truncateTitle
    >
      {!form ? (
        <p className="py-6 text-sm text-dim">Loading proposal...</p>
      ) : (
        <div className="flex flex-col gap-5">
          <p className="max-w-prose text-sm text-dim">
            This book&apos;s work is not on AudioSilo Meta yet. Complete the metadata below to
            propose it (an add-work intake issue). Only verifiable facts - leave a field blank if
            you are not sure. Fields marked * are required.
          </p>

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Field label="Title" required htmlFor="cp-title">
              <TextInput id="cp-title" value={form.title} onChange={(v) => update('title', v)} />
            </Field>
            <Field label="Subtitle" htmlFor="cp-subtitle">
              <TextInput
                id="cp-subtitle"
                value={form.subtitle}
                onChange={(v) => update('subtitle', v)}
              />
            </Field>
            <Field label="Authors (comma-separated)" required htmlFor="cp-authors">
              <TextInput
                id="cp-authors"
                value={form.authors}
                onChange={(v) => update('authors', v)}
              />
            </Field>
            <Field label="Narrators (comma-separated)" required htmlFor="cp-narrators">
              <TextInput
                id="cp-narrators"
                value={form.narrators}
                onChange={(v) => update('narrators', v)}
              />
            </Field>
            <Field label="Language" required htmlFor="cp-language">
              <TextInput
                id="cp-language"
                value={form.language}
                onChange={(v) => update('language', v)}
                placeholder="en"
              />
            </Field>
            <Field label="First published" htmlFor="cp-firstpub">
              <TextInput
                id="cp-firstpub"
                value={form.firstPublished}
                onChange={(v) => update('firstPublished', v)}
                placeholder="1998"
              />
            </Field>
            <Field label="Series name" htmlFor="cp-series">
              <TextInput
                id="cp-series"
                value={form.seriesName}
                onChange={(v) => update('seriesName', v)}
              />
            </Field>
            <Field label="Series position" htmlFor="cp-seriespos">
              <TextInput
                id="cp-seriespos"
                value={form.seriesPosition}
                onChange={(v) => update('seriesPosition', v)}
                placeholder="1"
              />
            </Field>
            <Field label="Runtime (minutes)" htmlFor="cp-runtime">
              <TextInput
                id="cp-runtime"
                type="number"
                value={form.runtimeMin}
                onChange={(v) => update('runtimeMin', v)}
              />
            </Field>
            <Field label="Abridged" htmlFor="cp-abridged">
              <select
                id="cp-abridged"
                value={form.abridged}
                onChange={(e) => update('abridged', e.target.value as AbridgedValue)}
                className="w-full rounded-md border border-edge bg-raised px-3 py-2 text-body"
              >
                <option value="">Unknown</option>
                <option value="Unabridged">Unabridged</option>
                <option value="Abridged">Abridged</option>
              </select>
            </Field>
            <Field label="Release date" htmlFor="cp-release">
              <TextInput
                id="cp-release"
                value={form.releaseDate}
                onChange={(v) => update('releaseDate', v)}
                placeholder="2005-06-21"
              />
            </Field>
            <Field label="Publisher" htmlFor="cp-publisher">
              <TextInput
                id="cp-publisher"
                value={form.publisher}
                onChange={(v) => update('publisher', v)}
              />
            </Field>
            <Field label="Print ISBNs (comma-separated)" htmlFor="cp-pisbn">
              <TextInput
                id="cp-pisbn"
                value={form.printIsbns}
                onChange={(v) => update('printIsbns', v)}
              />
            </Field>
            <Field label="Audiobook ISBNs (comma-separated)" htmlFor="cp-aisbn">
              <TextInput
                id="cp-aisbn"
                value={form.audiobookIsbns}
                onChange={(v) => update('audiobookIsbns', v)}
              />
            </Field>
            <Field label="Cover URL" htmlFor="cp-cover" className="sm:col-span-2">
              <TextInput
                id="cp-cover"
                value={form.coverUrl}
                onChange={(v) => update('coverUrl', v)}
              />
            </Field>
          </div>

          <div className="flex flex-col gap-2">
            <span className="text-sm font-medium text-hi">Audible ASINs</span>
            <div className="flex flex-col gap-2">
              {form.asins.map((row, i) => (
                <div key={i} className="flex items-center gap-2">
                  <select
                    aria-label={`ASIN region ${i + 1}`}
                    value={row.region}
                    onChange={(e) => setAsin(i, 'region', e.target.value)}
                    className="rounded-md border border-edge bg-raised px-2 py-2 text-body"
                  >
                    {ASIN_REGIONS.map((r) => (
                      <option key={r} value={r}>
                        {r}
                      </option>
                    ))}
                  </select>
                  <input
                    type="text"
                    aria-label={`ASIN ${i + 1}`}
                    value={row.asin}
                    placeholder="B0..."
                    onChange={(e) => setAsin(i, 'asin', e.target.value)}
                    className="flex-1 rounded-md border border-edge bg-raised px-3 py-2 text-body placeholder:text-dim"
                  />
                  <button
                    type="button"
                    onClick={() => removeAsinRow(i)}
                    aria-label={`Remove ASIN ${i + 1}`}
                    className="rounded-md border border-edge px-2 py-2 text-xs text-dim transition-colors hover:border-pink-600 hover:text-pink-400"
                  >
                    Remove
                  </button>
                </div>
              ))}
            </div>
            <button
              type="button"
              onClick={addAsinRow}
              className="w-max rounded-md border border-edge px-3 py-1.5 text-xs font-medium text-body transition-colors hover:border-pink-600 hover:text-hi"
            >
              Add ASIN
            </button>
          </div>

          <Field label="Sources" required htmlFor="cp-sources">
            <textarea
              id="cp-sources"
              value={form.sources}
              onChange={(e) => update('sources', e.target.value)}
              rows={3}
              placeholder="Where the facts came from (URLs, the audiobook's own metadata, etc.)"
              className="w-full rounded-md border border-edge bg-raised px-3 py-2 text-body placeholder:text-dim"
            />
          </Field>

          {error && (
            <p role="alert" className="text-sm text-pink-500">
              {error}
            </p>
          )}

          <div className="flex flex-wrap items-center gap-3">
            <button
              type="button"
              disabled={busy}
              onClick={() => void submitCore()}
              className="rounded-md bg-pink-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-pink-700 disabled:cursor-not-allowed disabled:opacity-50"
            >
              Submit proposal
            </button>
          </div>

          <div className="flex flex-col gap-2 border-t border-edge pt-4">
            <span className="text-sm font-medium text-hi">The work already exists</span>
            <p className="max-w-prose text-xs text-dim">
              If this book&apos;s work is in fact already on AudioSilo Meta, attach its slug
              instead. The book re-admits and contributes to that work.
            </p>
            <div className="flex flex-wrap items-center gap-2">
              <input
                type="text"
                aria-label="Existing work slug"
                value={workSlug}
                placeholder="the-work-slug"
                onChange={(e) => setWorkSlug(e.target.value)}
                className="flex-1 rounded-md border border-edge bg-raised px-3 py-2 text-body placeholder:text-dim"
              />
              <button
                type="button"
                disabled={busy}
                onClick={() => void attachExisting()}
                className="rounded-md border border-edge px-4 py-2 text-sm font-medium text-body transition-colors hover:border-pink-600 hover:text-hi disabled:cursor-not-allowed disabled:opacity-50"
              >
                Attach work
              </button>
            </div>
          </div>
        </div>
      )}
    </Modal>
  );
}

function TextInput({
  id,
  value,
  onChange,
  placeholder,
  type = 'text',
}: {
  id: string;
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  type?: string;
}) {
  return (
    <input
      id={id}
      type={type}
      value={value}
      placeholder={placeholder}
      onChange={(e) => onChange(e.target.value)}
      className="w-full rounded-md border border-edge bg-raised px-3 py-2 text-body placeholder:text-dim"
    />
  );
}
