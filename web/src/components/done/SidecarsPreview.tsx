import { useEffect, useState, type ReactNode } from 'react';
import { ApiError } from '@/lib/apiClient';
import type { Character, SidecarsView } from '@/api/types';
import { revealLabel, roleLabel, storyRows, type StoryRow as StoryRowData } from '@/lib/expressive';

interface SidecarsPreviewProps {
  title: string;
  // Fetches the extracted sidecars for the book. Kept as a prop so the modal stays
  // decoupled from ApiClient (mirrors BookRow's getDetail). 404 -> a quiet note.
  getSidecars: () => Promise<SidecarsView>;
  onClose: () => void;
}

// SidecarsPreview renders the extracted characters/recaps the way
// meta.audiosilo.app does: a modal with Characters / Story so far tabs of
// spoiler-gated cards. The card structure is ported from audiosilo-meta
// site/src/components/detail/WorkDetail.tsx; the label/ordering logic is the
// vendored expressive.ts (roleLabel / revealLabel / storyRows / sortRecaps).
export function SidecarsPreview({ title, getSidecars, onClose }: SidecarsPreviewProps) {
  const [state, setState] = useState<'loading' | 'ready' | 'empty' | 'error'>('loading');
  const [view, setView] = useState<SidecarsView | null>(null);
  const [tab, setTab] = useState<'characters' | 'recaps'>('characters');

  useEffect(() => {
    let cancelled = false;
    getSidecars()
      .then((res) => {
        if (cancelled) return;
        setView(res);
        setState('ready');
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // 404 = the pipeline produced no sidecar files for this book (a quiet,
        // expected state), distinct from a real fetch error.
        if (err instanceof ApiError && err.status === 404) {
          setState('empty');
        } else {
          setState('error');
        }
      });
    return () => {
      cancelled = true;
    };
  }, [getSidecars]);

  const characters = view?.characters ?? [];
  const recapRows = storyRows(view?.recaps ?? [], view?.recap_summary);
  const hasCharacters = characters.length > 0;
  const hasRecaps = recapRows.length > 0;

  // Open on whichever tab has content (characters first). Runs once data lands.
  useEffect(() => {
    if (state !== 'ready') return;
    setTab(hasCharacters ? 'characters' : 'recaps');
  }, [state, hasCharacters]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/60 p-4 sm:p-8"
      role="dialog"
      aria-modal="true"
      aria-label={`Sidecars for ${title}`}
      onClick={onClose}
    >
      <div
        className="mt-8 w-full max-w-3xl rounded-xl border border-edge bg-surface p-5 shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="mb-4 flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h3 className="text-base font-medium text-hi">Sidecars preview</h3>
            <p className="mt-0.5 truncate text-xs text-dim">{title}</p>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="rounded p-1 text-dim transition-colors hover:text-hi"
          >
            &#10005;
          </button>
        </div>

        {state === 'loading' && <p className="py-6 text-sm text-dim">Loading sidecars...</p>}
        {state === 'error' && (
          <p role="alert" className="py-6 text-sm text-pink-500">
            The sidecars could not be loaded.
          </p>
        )}
        {state === 'empty' && (
          <p className="py-6 text-sm text-dim">No sidecars produced for this book.</p>
        )}
        {state === 'ready' && !hasCharacters && !hasRecaps && (
          <p className="py-6 text-sm text-dim">No sidecars produced for this book.</p>
        )}

        {state === 'ready' && (hasCharacters || hasRecaps) && (
          <>
            <div
              role="tablist"
              aria-label="Sidecar sections"
              className="flex gap-6 border-b border-edge"
            >
              {hasCharacters && (
                <TabButton
                  id="sc-tab-characters"
                  controls="sc-panel-characters"
                  active={tab === 'characters'}
                  onClick={() => setTab('characters')}
                  label="Characters"
                  count={characters.length}
                />
              )}
              {hasRecaps && (
                <TabButton
                  id="sc-tab-recaps"
                  controls="sc-panel-recaps"
                  active={tab === 'recaps'}
                  onClick={() => setTab('recaps')}
                  label="Story so far"
                  count={recapRows.length}
                />
              )}
            </div>

            {tab === 'characters' && hasCharacters && (
              <div role="tabpanel" id="sc-panel-characters" aria-labelledby="sc-tab-characters">
                <CharactersPanel characters={characters} />
              </div>
            )}
            {tab === 'recaps' && hasRecaps && (
              <div role="tabpanel" id="sc-panel-recaps" aria-labelledby="sc-tab-recaps">
                <RecapsPanel rows={recapRows} />
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}

// --- Ported card components (from audiosilo-meta WorkDetail.tsx) ---

// The shared disclosure chevron for the accordions. Points down when closed and
// rotates 180deg when open.
function Chevron({ open, className }: { open: boolean; className?: string }) {
  return (
    <svg
      className={`h-4 w-4 text-dim transition-transform ${open ? 'rotate-180' : ''}${
        className ? ` ${className}` : ''
      }`}
      xmlns="http://www.w3.org/2000/svg"
      fill="none"
      viewBox="0 0 24 24"
      strokeWidth={1.5}
      stroke="currentColor"
      aria-hidden="true"
    >
      <path strokeLinecap="round" strokeLinejoin="round" d="m19.5 8.25-7.5 7.5-7.5-7.5" />
    </svg>
  );
}

// A small uppercase pill used for character roles and recap scopes.
function Badge({ children }: { children: ReactNode }) {
  return (
    <span className="shrink-0 rounded-full border border-edge bg-raised px-2 py-0.5 text-[0.65rem] uppercase tracking-wide text-dim">
      {children}
    </span>
  );
}

// One character card. The description is spoiler-bounded to the reveal position but
// still story detail, so it stays hidden behind a per-card accordion closed by
// default. The name stays a real heading in both branches; a card with no
// description has no disclosure control.
function CharacterCard({ character }: { character: Character }) {
  const [open, setOpen] = useState(false);
  const role = roleLabel(character.role);
  const hasDescription = Boolean(character.description);
  const descId = `sc-char-desc-${character.id}`;
  const reveal = (
    <span className="text-xs font-medium text-pink-400/90">{revealLabel(character.reveal)}</span>
  );

  return (
    <article className="overflow-hidden rounded-2xl border border-edge bg-surface p-4">
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <h4 className="font-semibold text-hi">{character.name}</h4>
          {character.aliases && character.aliases.length > 0 ? (
            <p className="mt-0.5 text-xs text-dim">also {character.aliases.join(', ')}</p>
          ) : null}
        </div>
        {role ? <Badge>{role}</Badge> : null}
      </div>
      {hasDescription ? (
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          aria-expanded={open}
          aria-controls={descId}
          className="mt-2 flex w-full items-center justify-between gap-2 text-left transition-colors hover:opacity-80"
        >
          {reveal}
          <Chevron open={open} className="shrink-0" />
        </button>
      ) : (
        <p className="mt-2">{reveal}</p>
      )}
      {hasDescription && open ? (
        <p id={descId} className="mt-3 text-sm leading-relaxed text-body">
          {character.description}
        </p>
      ) : null}
    </article>
  );
}

// The cast of a work: spoiler-aware character cards, each a per-card accordion so
// descriptions stay hidden until opened.
function CharactersPanel({ characters }: { characters: Character[] }) {
  return (
    <>
      <p className="mt-4 max-w-2xl text-sm text-dim">
        Community-written and spoiler-aware - open a character to read who they are, scoped to where
        they first appear.
      </p>
      <div className="mt-4 grid gap-4 sm:grid-cols-2">
        {characters.map((c) => (
          <CharacterCard key={c.id} character={c} />
        ))}
      </div>
    </>
  );
}

// One "story so far" row: a collapsible accordion closed by default (spoiler-safe)
// until the reader opens it. Shared by the position-keyed chaptered recaps and the
// whole-book summary rows.
function StoryRow({ title, badge, text }: { title: string; badge?: string; text: string }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="bg-surface">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        className="flex w-full items-center justify-between gap-3 px-4 py-3 text-left transition-colors hover:bg-raised/40"
      >
        <span className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-medium text-body">{title}</span>
          {badge ? <Badge>{badge}</Badge> : null}
        </span>
        <Chevron open={open} className="shrink-0" />
      </button>
      {open ? <p className="px-4 pb-4 text-sm leading-relaxed text-body">{text}</p> : null}
    </div>
  );
}

// "Story so far": the rows built by storyRows, every one an accordion closed by
// default so the reader chooses how far to reveal.
function RecapsPanel({ rows }: { rows: StoryRowData[] }) {
  const hasWholeBook = rows.some((r) => r.wholeBook);
  return (
    <>
      <p className="mt-4 max-w-2xl text-sm text-dim">
        Open a recap only as far as you have listened
        {hasWholeBook ? ' - the whole-book rows are full spoilers.' : '.'}
      </p>
      <div className="mt-4 divide-y divide-edge/60 overflow-hidden rounded-2xl border border-edge">
        {rows.map((row, i) => (
          <StoryRow key={`${row.title}-${i}`} {...row} />
        ))}
      </div>
    </>
  );
}

// One tab: an underlined active state in the accent, with an optional muted count.
function TabButton({
  active,
  onClick,
  label,
  count,
  id,
  controls,
}: {
  active: boolean;
  onClick: () => void;
  label: string;
  count?: number;
  id: string;
  controls: string;
}) {
  return (
    <button
      type="button"
      role="tab"
      id={id}
      aria-selected={active}
      aria-controls={controls}
      onClick={onClick}
      className={`-mb-px flex items-center gap-1.5 border-b-2 px-1 pb-3 pt-1 text-sm font-medium transition-colors ${
        active ? 'border-pink-500 text-hi' : 'border-transparent text-dim hover:text-body'
      }`}
    >
      <span>{label}</span>
      {typeof count === 'number' ? (
        <span className="text-xs font-normal text-dim">{count}</span>
      ) : null}
    </button>
  );
}
