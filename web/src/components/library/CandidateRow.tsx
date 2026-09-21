import { memo } from 'react';
import type { PipelineBookRef, ScannedBook } from '@/api/types';
import {
  currentForceAudio,
  isManualMatch,
  isPipelineDone,
  matchProvenanceLabel,
} from '@/lib/candidates';
import { stateLabel } from '@/lib/pipelineState';
import { CoverageBadge } from './CoverageBadge';

interface CandidateRowProps {
  book: ScannedBook;
  checked: boolean;
  onToggle: (path: string, checked: boolean) => void;
  // Row actions. A hidden row shows Unhide; a visible row shows Hide + Match (and
  // Clear match when the book carries a manual match). Omitted callbacks hide the
  // corresponding control.
  onMatch?: (book: ScannedBook) => void;
  onClearMatch?: (book: ScannedBook) => void;
  onHide?: (book: ScannedBook) => void;
  onUnhide?: (book: ScannedBook) => void;
  // Toggles force_audio for a HYBRID candidate (an epub beside an audiobook), so
  // the audio runs when the epub is the wrong edition or an abridgement. Never
  // offered for an ebook-only row: there is no audio to fall back to.
  onToggleSource?: (book: ScannedBook, forceAudio: boolean) => void;
  // Shows the "New" pill on a book the server flagged is_new. Set only in the All
  // view - in the New view every row is new, so the pill would be noise.
  markNew?: boolean;
  // Disables this row's actions while one of its overrides is in flight.
  busy?: boolean;
}

// A provenance-labelled identity chip (ASIN/ISBN). The title tooltip surfaces
// where the value came from (tag / path / filename) via the scan's sources map.
function IdentityChip({
  kind,
  value,
  source,
}: {
  kind: 'ASIN' | 'ISBN';
  value: string;
  source: string | undefined;
}) {
  const provenance = source ? ` (from ${source})` : '';
  return (
    <span
      title={`${kind} ${value}${provenance}`}
      className="inline-flex items-center gap-1 rounded border border-edge bg-raised px-1.5 py-0.5 font-mono text-[10px] text-dim"
    >
      <span className="text-[9px] font-semibold uppercase tracking-wide text-body">{kind}</span>
      {value}
    </span>
  );
}

// BADGE_CLASS is the one chrome the row's status pills share (New, the pipeline
// presence, EPUB); each caller adds only its own border/background/text colours.
const BADGE_CLASS =
  'inline-flex rounded border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide';

// Badge is a small uppercase status pill, following CoverageBadge's local Pill:
// one class string, so the three pills cannot drift apart visually.
function Badge({
  className,
  title,
  children,
}: {
  className: string;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <span title={title} className={BADGE_CLASS + ' ' + className}>
      {children}
    </span>
  );
}

function RowButton({
  onClick,
  disabled,
  children,
  title,
}: {
  onClick: () => void;
  disabled?: boolean;
  children: React.ReactNode;
  title?: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      title={title}
      className="rounded border border-edge bg-raised px-2 py-1 text-xs text-body transition-colors hover:bg-edge disabled:cursor-not-allowed disabled:opacity-50"
    >
      {children}
    </button>
  );
}

// CandidateRow is memoized. To be clear about what that does and does NOT buy:
// while a scan is running, every ~700ms poll parses a fresh JSON payload, so each
// book object is a new reference and the memo does NOT skip (identities change
// every tick). The memo pays off once polling stops - then a re-render driven by a
// selection toggle, a busy-state change, or a hide/match action only reconciles the
// rows whose own props actually changed, instead of every row. It relies on the
// parent passing stable callbacks (onToggle et al. are bound once) so an unchanged
// row's props stay referentially equal.
export const CandidateRow = memo(function CandidateRow({
  book,
  checked,
  onToggle,
  onMatch,
  onClearMatch,
  onHide,
  onToggleSource,
  onUnhide,
  markNew = false,
  busy = false,
}: CandidateRowProps) {
  const authors = (book.authors ?? []).join(', ');
  const seriesText =
    book.series && book.series_position
      ? `${book.series} #${book.series_position}`
      : (book.series ?? '');

  const runtimeText =
    book.runtime_min && book.runtime_min > 0 ? formatRuntime(book.runtime_min) : '';
  const chapterText = book.chapters && book.chapters > 0 ? `${book.chapters} ch` : '';

  const hidden = !!book.hidden;
  const isEbook = book.kind === 'ebook';
  // A HYBRID row has an epub AND an audiobook: source_path is the folder while
  // ebook_path names the file inside it. Only such a row can fall back to audio.
  const hybrid = !!book.ebook_path && book.ebook_path !== book.source_path;
  // Shared with the value the toggle SENDS (overridePayload), so the button's label
  // and the request it makes cannot drift apart.
  const forcedAudio = currentForceAudio(book);
  const pipelineBook = book.pipeline_book;
  const provenance = matchProvenanceLabel(book.coverage);
  const manual = isManualMatch(book.coverage);
  const pipeline = pipelineBook ? pipelinePresence(pipelineBook) : null;

  return (
    <tr
      className={
        'border-t border-edge align-top hover:bg-raised/40' + (hidden ? ' opacity-60' : '')
      }
    >
      <td className="px-3 py-3">
        {pipelineBook ? (
          <span className="text-dim" aria-hidden="true">
            -
          </span>
        ) : (
          <input
            type="checkbox"
            checked={checked}
            disabled={hidden}
            onChange={(e) => onToggle(book.path, e.target.checked)}
            aria-label={`Select ${book.title}`}
            className="mt-0.5 h-4 w-4 accent-pink-600 disabled:cursor-not-allowed disabled:opacity-40"
          />
        )}
      </td>
      <td className="px-3 py-3">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-medium text-hi">{book.title}</span>
          {markNew && book.is_new && (
            <Badge className="border-sky-500/40 bg-sky-500/10 text-sky-300" title={newTitle(book)}>
              New
            </Badge>
          )}
          {pipeline && pipelineBook && (
            <Badge
              className={pipeline.className}
              title={`Pipeline book #${pipelineBook.id}: ${stateLabel(pipelineBook.state)}`}
            >
              {pipeline.label}
            </Badge>
          )}
          {isEbook && (
            <Badge
              className="border-pink-600/40 bg-pink-600/10 text-pink-400"
              title={
                hybrid
                  ? 'An epub sits beside this audiobook, so its exact text is used - no transcription needed'
                  : 'This book is an epub; its text is used directly'
              }
            >
              {hybrid ? 'EPUB + audio' : 'EPUB'}
            </Badge>
          )}
        </div>
        {book.ebook_note && <div className="mt-0.5 text-xs italic text-dim">{book.ebook_note}</div>}
        {book.subtitle && <div className="text-xs text-dim">{book.subtitle}</div>}
        {authors && <div className="text-xs text-body">{authors}</div>}
        {/* Where the row actually lives. A generic title ("Jack Reacher Short Story
            #02") is unidentifiable without it. The max-w is load-bearing: the parent
            table is auto-layout inside an overflow-x-auto wrapper, so a bare
            `truncate` (which sets whitespace-nowrap) would make the cell grow to
            max-content and scroll the table sideways instead of clipping. Capping the
            div's width bounds the cell's max-content contribution AND gives the
            ellipsis something to clip against. The absolute path is the tooltip. */}
        {book.path && (
          <div
            className="mt-0.5 max-w-[24rem] truncate font-mono text-[11px] text-dim"
            title={book.source_path || book.path}
          >
            {book.path}
          </div>
        )}
        <div className="mt-1 flex flex-wrap items-center gap-1.5">
          {book.asin && <IdentityChip kind="ASIN" value={book.asin} source={book.sources?.asin} />}
          {book.isbn && <IdentityChip kind="ISBN" value={book.isbn} source={book.sources?.isbn} />}
        </div>
      </td>
      <td className="px-3 py-3 text-sm text-body">
        {seriesText || <span className="text-dim">-</span>}
      </td>
      <td className="px-3 py-3 text-sm text-body">
        {runtimeText || chapterText ? (
          <div className="flex flex-col">
            {runtimeText && <span>{runtimeText}</span>}
            {chapterText && <span className="text-xs text-dim">{chapterText}</span>}
          </div>
        ) : (
          <span className="text-dim">-</span>
        )}
      </td>
      <td className="px-3 py-3">
        <CoverageBadge coverage={book.coverage} />
        {provenance && (
          <div className="mt-1 text-[11px] italic text-dim" title={provenance}>
            {provenance}
          </div>
        )}
      </td>
      <td className="px-3 py-3">
        <div className="flex flex-wrap justify-end gap-1.5">
          {!hidden && hybrid && onToggleSource && (
            <RowButton
              onClick={() => onToggleSource(book, !forcedAudio)}
              disabled={busy}
              title={
                forcedAudio
                  ? 'Use the epub text instead of transcribing the audio'
                  : 'Transcribe the audio instead of using the epub (for a wrong edition or an abridgement)'
              }
            >
              {forcedAudio ? 'Use epub' : 'Use audio'}
            </RowButton>
          )}
          {hidden ? (
            onUnhide && (
              <RowButton
                onClick={() => onUnhide(book)}
                disabled={busy}
                title="Show this book again"
              >
                Unhide
              </RowButton>
            )
          ) : (
            <>
              {!pipelineBook && onMatch && (
                <RowButton
                  onClick={() => onMatch(book)}
                  disabled={busy}
                  title="Match this book against meta.audiosilo.app"
                >
                  Match
                </RowButton>
              )}
              {!pipelineBook && manual && onClearMatch && (
                <RowButton
                  onClick={() => onClearMatch(book)}
                  disabled={busy}
                  title="Remove the manual match"
                >
                  Clear match
                </RowButton>
              )}
              {onHide && (
                <RowButton
                  onClick={() => onHide(book)}
                  disabled={busy}
                  title="Hide this book from the list"
                >
                  Hide
                </RowButton>
              )}
            </>
          )}
        </div>
      </td>
    </tr>
  );
});

// newTitle is the New pill's tooltip. first_seen_at is the whole point of the
// sightings table, so the pill states the date it names (localised - the value is
// RFC3339 UTC on the wire); a book recorded before the field existed falls back to
// the generic wording rather than showing "Invalid Date".
function newTitle(book: ScannedBook): string {
  if (book.first_seen_at) {
    return `First seen ${new Date(book.first_seen_at).toLocaleDateString()}`;
  }
  return 'First seen by a recent scan - not yet processed or dismissed';
}

function pipelinePresence(pipelineBook: PipelineBookRef): { label: string; className: string } {
  // Shared with the Library filter's "already processed" rule, so a finished book
  // cannot read as Completed here and still count as a candidate there.
  if (isPipelineDone(pipelineBook)) {
    return { label: 'Completed', className: 'border-success/40 bg-success/10 text-success' };
  }
  switch (pipelineBook.status) {
    case 'paused':
      return {
        label: 'Paused',
        className: 'border-amber-500/40 bg-amber-500/10 text-amber-300',
      };
    case 'needs_attention':
      return {
        label: 'Needs attention',
        className: 'border-orange-500/40 bg-orange-500/10 text-orange-300',
      };
    case 'failed':
      return {
        label: 'Failed',
        className: 'border-pink-500/40 bg-pink-500/10 text-pink-300',
      };
    default:
      return {
        label: 'In queue',
        className: 'border-sky-500/40 bg-sky-500/10 text-sky-300',
      };
  }
}

function formatRuntime(minutes: number): string {
  const h = Math.floor(minutes / 60);
  const m = minutes % 60;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}
