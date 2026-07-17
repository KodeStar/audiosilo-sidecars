import { useCallback, useState } from 'react';

export type LazyDetailState = 'idle' | 'loading' | 'error';

export interface LazyDetail<T> {
  // Whether the row is expanded (its detail panel shown).
  expanded: boolean;
  // Toggle the expansion. On opening it (re)fetches the detail so the panel
  // reflects the latest data; collapsing does not fetch.
  toggle: () => void;
  // The last-fetched detail, or null before the first successful fetch.
  detail: T | null;
  // The fetch status for the panel to render a loading/error affordance.
  detailState: LazyDetailState;
  // Re-fetch the detail while already expanded (a no-op when collapsed). For a
  // caller that wants to refresh the open panel after an external change.
  refresh: () => void;
}

// useLazyDetail drives the "expand a row to lazily load its detail" pattern shared
// by the Running and Done rows: an expanded flag, the fetched detail, and an
// idle/loading/error state. The detail is fetched only when the row is opened, and
// re-fetched on each open so it stays current. Kept framework-thin so the rows stay
// thin over it.
export function useLazyDetail<T>(getDetail: (id: number) => Promise<T>, id: number): LazyDetail<T> {
  const [expanded, setExpanded] = useState(false);
  const [detail, setDetail] = useState<T | null>(null);
  const [detailState, setDetailState] = useState<LazyDetailState>('idle');

  const fetchDetail = useCallback(async () => {
    setDetailState('loading');
    try {
      setDetail(await getDetail(id));
      setDetailState('idle');
    } catch {
      setDetailState('error');
    }
  }, [getDetail, id]);

  const toggle = useCallback(() => {
    const next = !expanded;
    setExpanded(next);
    if (next) void fetchDetail();
  }, [expanded, fetchDetail]);

  const refresh = useCallback(() => {
    if (expanded) void fetchDetail();
  }, [expanded, fetchDetail]);

  return { expanded, toggle, detail, detailState, refresh };
}
