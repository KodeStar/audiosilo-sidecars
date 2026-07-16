// The Library tab's scan + selection store, lifted OUT of the React component so
// it survives tab switches (AppShell unmounts inactive panels, but the daemon
// scan keeps running - the old design lost the UI state and looked like a cancel).
//
// It is a framework-free class (fully unit-testable by driving it with a fake
// client + an injected timer) with a module-level singleton `scanStore` exposed
// to React via `useScanStore()` (useSyncExternalStore). The 700ms poll loop lives
// here, is idempotent/re-entrant safe (a generation counter invalidates in-flight
// ticks), and stops on a terminal status. Optimistic hide / manual-match patches
// are kept in an overlay and re-applied over each polled `books` array so a later
// poll can't drop them.

import { useSyncExternalStore } from 'react';
import { ApiError, type ApiClient } from '@/lib/apiClient';
import type { BookCandidate, Coverage, ScanJob, ScannedBook, SetOverrideBody } from '@/api/types';
import { clearedCoverage, overridePayload, summarizeTally, tallyResults } from '@/lib/candidates';

const DEFAULT_POLL_MS = 700;

export interface ScanNote {
  kind: 'ok' | 'error';
  text: string;
}

// The immutable snapshot React renders. `job.books` already has the optimistic
// overlay applied. Everything here must survive a tab switch.
export interface ScanState {
  job: ScanJob | null;
  scanError: string | null;
  starting: boolean;
  excludeCovered: boolean;
  showHidden: boolean;
  selected: ReadonlySet<string>;
  processing: boolean;
  note: ScanNote | null;
}

// BookPatch is the per-book optimistic overlay applied over the polled book.
export interface BookPatch {
  hidden?: boolean;
  coverage?: Coverage;
}

// mergeBooks applies the optimistic overlay (keyed by absolute source_path, the
// same key the daemon's overrides use) onto the polled book list. Pure so it is
// exhaustively testable; order is preserved and unknown paths pass through
// untouched.
export function mergeBooks(
  books: ScannedBook[],
  patches: ReadonlyMap<string, BookPatch>,
): ScannedBook[] {
  if (patches.size === 0) return books;
  return books.map((b) => {
    const patch = patches.get(b.source_path);
    if (!patch) return b;
    return {
      ...b,
      ...(patch.hidden !== undefined ? { hidden: patch.hidden } : {}),
      ...(patch.coverage !== undefined ? { coverage: patch.coverage } : {}),
    };
  });
}

// scanErrorMessage maps a caught error to a friendly scan message (the 403
// library-roots case is common enough to spell out).
export function scanErrorMessage(err: unknown, fallback = 'Could not reach the daemon.'): string {
  if (err instanceof ApiError && err.status === 403) {
    return 'That path is outside the configured library roots. Set library_roots in the daemon config to allow it.';
  }
  if (err instanceof ApiError) return err.message;
  return fallback;
}

// overrideErrorMessage maps a caught override error to a friendly message.
export function overrideErrorMessage(err: unknown): string {
  if (err instanceof ApiError && err.status === 403) {
    return 'That path is outside the configured library roots.';
  }
  if (err instanceof ApiError) return err.message;
  return 'Could not update the book.';
}

type Timer = ReturnType<typeof setTimeout>;

export interface ScanStoreOptions {
  pollMs?: number;
  setTimeoutFn?: (cb: () => void, ms: number) => Timer;
  clearTimeoutFn?: (t: Timer) => void;
}

// The outcome of a process() call, so the component can switch tabs on success.
export interface ProcessOutcome {
  started: boolean;
}

const INITIAL: ScanState = {
  job: null,
  scanError: null,
  starting: false,
  excludeCovered: false,
  showHidden: false,
  selected: new Set(),
  processing: false,
  note: null,
};

export class ScanStore {
  private state: ScanState = INITIAL;
  private readonly listeners = new Set<() => void>();

  // The last polled book array (pre-overlay) + the optimistic overlay, kept apart
  // so a fresh poll re-derives job.books = mergeBooks(rawBooks, patches).
  private rawBooks: ScannedBook[] = [];
  private patches = new Map<string, BookPatch>();

  private pollTimer: Timer | null = null;
  private pollGen = 0;
  private reattachStarted = false;

  private readonly pollMs: number;
  private readonly setTimeoutFn: (cb: () => void, ms: number) => Timer;
  private readonly clearTimeoutFn: (t: Timer) => void;

  constructor(opts: ScanStoreOptions = {}) {
    this.pollMs = opts.pollMs ?? DEFAULT_POLL_MS;
    this.setTimeoutFn = opts.setTimeoutFn ?? ((cb, ms) => setTimeout(cb, ms));
    this.clearTimeoutFn = opts.clearTimeoutFn ?? ((t) => clearTimeout(t));
  }

  // --- external-store plumbing ---

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getSnapshot = (): ScanState => this.state;

  private set(partial: Partial<ScanState>): void {
    this.state = { ...this.state, ...partial };
    for (const l of this.listeners) l();
  }

  // --- preferences ---

  setExcludeCovered(v: boolean): void {
    this.set({ excludeCovered: v });
  }

  setShowHidden(v: boolean): void {
    this.set({ showHidden: v });
  }

  // --- selection ---

  toggleOne(path: string, on: boolean): void {
    const next = new Set(this.state.selected);
    if (on) next.add(path);
    else next.delete(path);
    this.set({ selected: next });
  }

  // toggleAll adds/removes exactly the given paths (the caller passes the visible,
  // selectable subset - hidden books are never passed in).
  toggleAll(paths: readonly string[], on: boolean): void {
    const next = new Set(this.state.selected);
    for (const p of paths) {
      if (on) next.add(p);
      else next.delete(p);
    }
    this.set({ selected: next });
  }

  private deselect(path: string): void {
    if (!this.state.selected.has(path)) return;
    const next = new Set(this.state.selected);
    next.delete(path);
    this.set({ selected: next });
  }

  // --- scan lifecycle ---

  // startScan returns { ok } reflecting whether createScan was ACCEPTED (the scan
  // then polls in the background). The caller uses it to decide follow-up actions
  // - e.g. only recording the folder as a recent root once the daemon accepted it,
  // not on a 403 rejection.
  async startScan(client: ApiClient, path: string): Promise<{ ok: boolean }> {
    const trimmed = path.trim();
    if (trimmed === '' || this.state.starting) return { ok: false };
    this.stopPoll();
    this.rawBooks = [];
    this.patches.clear();
    this.set({
      starting: true,
      scanError: null,
      note: null,
      job: null,
      selected: new Set(),
    });
    try {
      const { job_id } = await client.createScan(trimmed);
      this.set({ starting: false });
      this.beginPoll(client, job_id);
      return { ok: true };
    } catch (err) {
      this.set({ starting: false, scanError: scanErrorMessage(err) });
      return { ok: false };
    }
  }

  // reattach adopts the most recent daemon scan after a fresh page load, once per
  // session. On an in-session tab switch the module state already holds the job,
  // so no network call is made.
  async reattach(client: ApiClient): Promise<void> {
    if (this.reattachStarted) return;
    this.reattachStarted = true;
    if (this.state.job || this.state.starting) return;
    try {
      const { scans } = await client.listScans();
      if (scans.length === 0) return;
      // A scan may have started in the gap while listScans was in flight.
      if (this.state.job || this.state.starting) return;
      const summary = scans[0];
      this.rawBooks = [];
      this.patches.clear();
      this.set({ job: { ...summary, books: [] } });
      // Poll once (even a done scan) to fetch its books, then continue if running.
      this.beginPoll(client, summary.id);
    } catch {
      // Reattach is best-effort; a failure just leaves the tab empty. Reset the
      // latch so a later mount retries after a transient failure (the daemon
      // briefly unreachable) rather than staying empty for the whole session.
      this.reattachStarted = false;
    }
  }

  // clearForNewScan resets the scan/selection state (keeping the user's toggle
  // preferences) and stops any poll. startScan already does this, so this is for
  // an explicit "clear" affordance.
  clearForNewScan(): void {
    this.stopPoll();
    this.rawBooks = [];
    this.patches.clear();
    this.set({
      job: null,
      scanError: null,
      starting: false,
      selected: new Set(),
      processing: false,
      note: null,
    });
  }

  // reset returns the store to its initial state and drops the reattach latch, so
  // a fresh session (after sign-out) starts clean and can reattach again. Unlike
  // clearForNewScan it also discards the toggle preferences and re-arms reattach.
  reset(): void {
    this.stopPoll();
    this.rawBooks = [];
    this.patches.clear();
    this.reattachStarted = false;
    this.set(INITIAL);
  }

  private stopPoll(): void {
    if (this.pollTimer !== null) {
      this.clearTimeoutFn(this.pollTimer);
      this.pollTimer = null;
    }
    // Bump the generation so any in-flight tick's continuation is a no-op.
    this.pollGen++;
  }

  private beginPoll(client: ApiClient, jobId: string): void {
    this.stopPoll();
    const gen = this.pollGen;
    const tick = async (): Promise<void> => {
      if (gen !== this.pollGen) return;
      let job: ScanJob;
      try {
        job = await client.getScan(jobId);
      } catch (err) {
        if (gen !== this.pollGen) return;
        this.set({ scanError: scanErrorMessage(err, 'Could not read the scan status.') });
        return;
      }
      if (gen !== this.pollGen) return;
      this.applyPolledJob(job);
      if (job.status === 'running') {
        this.pollTimer = this.setTimeoutFn(() => void tick(), this.pollMs);
      }
    };
    void tick();
  }

  private applyPolledJob(job: ScanJob): void {
    this.rawBooks = job.books ?? [];
    this.set({ job: { ...job, books: mergeBooks(this.rawBooks, this.patches) } });
  }

  private recompute(): void {
    if (!this.state.job) return;
    this.set({ job: { ...this.state.job, books: mergeBooks(this.rawBooks, this.patches) } });
  }

  // --- processing (enqueue selected books) ---

  async process(client: ApiClient, candidates: BookCandidate[]): Promise<ProcessOutcome> {
    if (candidates.length === 0 || this.state.processing) return { started: false };
    this.set({ processing: true, note: null });
    try {
      const { results } = await client.createBooks(candidates);
      const tally = tallyResults(results);
      this.set({ processing: false, selected: new Set() });
      if (tally.created > 0) return { started: true };
      this.set({
        note: {
          kind: tally.conflicts > 0 || tally.failed > 0 ? 'error' : 'ok',
          text: summarizeTally(tally),
        },
      });
      return { started: false };
    } catch (err) {
      this.set({
        processing: false,
        note: {
          kind: 'error',
          text: err instanceof ApiError ? err.message : 'Could not enqueue the selected books.',
        },
      });
      return { started: false };
    }
  }

  // --- overrides (hide / manual match) ---

  hide(client: ApiClient, book: ScannedBook): Promise<{ ok: boolean; error?: string }> {
    return this.applyOverride(client, book, overridePayload(book, { hidden: true }), 'keep');
  }

  unhide(client: ApiClient, book: ScannedBook): Promise<{ ok: boolean; error?: string }> {
    return this.applyOverride(client, book, overridePayload(book, { hidden: false }), 'keep');
  }

  applyManualMatch(
    client: ApiClient,
    book: ScannedBook,
    workId: string,
  ): Promise<{ ok: boolean; error?: string }> {
    return this.applyOverride(client, book, overridePayload(book, { workId }), 'fromResponse');
  }

  clearMatch(client: ApiClient, book: ScannedBook): Promise<{ ok: boolean; error?: string }> {
    return this.applyOverride(client, book, overridePayload(book, { workId: '' }), 'clear');
  }

  // applyOverride POSTs the full desired override, then reconciles the local
  // overlay from the response. The overlay keys on the absolute source_path (the
  // daemon's override key); the selection keys on the in-scan relative path.
  // coverageMode decides what happens to the book's coverage: 'keep' leaves it
  // (hide/unhide), 'fromResponse' adopts the recomputed manual coverage, 'clear'
  // reverts it to unknown (match cleared).
  private async applyOverride(
    client: ApiClient,
    book: ScannedBook,
    body: SetOverrideBody,
    coverageMode: 'keep' | 'fromResponse' | 'clear',
  ): Promise<{ ok: boolean; error?: string }> {
    try {
      const res = await client.setOverride(body);
      const prev = this.patches.get(body.source_path) ?? {};
      const patch: BookPatch = { ...prev, hidden: res.override.hidden };
      if (coverageMode === 'fromResponse' && res.coverage) {
        patch.coverage = res.coverage;
      } else if (coverageMode === 'clear') {
        patch.coverage = clearedCoverage();
      }
      this.patches.set(body.source_path, patch);
      if (res.override.hidden) this.deselect(book.path);
      this.recompute();
      return { ok: true };
    } catch (err) {
      return { ok: false, error: overrideErrorMessage(err) };
    }
  }
}

// The app-wide singleton the React tree binds to.
export const scanStore = new ScanStore();

// useScanStore subscribes a component to the singleton's snapshot.
export function useScanStore(): ScanState {
  return useSyncExternalStore(scanStore.subscribe, scanStore.getSnapshot, scanStore.getSnapshot);
}
