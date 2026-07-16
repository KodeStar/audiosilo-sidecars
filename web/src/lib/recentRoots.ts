// A tiny localStorage-backed list of recently-scanned folder roots, so the
// Library tab can offer a "recent roots" dropdown (the daemon is local, so a
// plain path + history is the right affordance - no native picker in a web UI).

const STORAGE_KEY = 'audiosilo.sidecars.recentRoots';
const MAX_ROOTS = 8;

// loadRecentRoots returns the stored roots, most-recent first. It never throws:
// a missing/corrupt entry yields an empty list.
export function loadRecentRoots(): string[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((v): v is string => typeof v === 'string');
  } catch {
    return [];
  }
}

// addRecentRoot pushes path to the front (de-duplicated, capped), persists the
// list, and returns it. An empty/whitespace path is ignored.
export function addRecentRoot(path: string): string[] {
  const trimmed = path.trim();
  if (trimmed === '') return loadRecentRoots();
  const existing = loadRecentRoots().filter((p) => p !== trimmed);
  const next = [trimmed, ...existing].slice(0, MAX_ROOTS);
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
  } catch {
    // Storage is best-effort; a quota/availability failure is non-fatal.
  }
  return next;
}
