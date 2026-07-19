import type { SystemTab } from '@/api/types';

const TAB_PARAM = 'tab';

// readTabFromURL returns the requested deep-linked tab, if present.
export function readTabFromURL(search = window.location.search): string | null {
  const value = new URLSearchParams(search).get(TAB_PARAM)?.trim();
  return value || null;
}

// resolveTab accepts a URL tab only when the daemon currently exposes it as
// ready. Otherwise it preserves the existing first-ready fallback.
export function resolveTab(tabs: SystemTab[], requested: string | null): string {
  const linked = tabs.find((tab) => tab.id === requested && tab.status === 'ready');
  const ready = tabs.find((tab) => tab.status === 'ready');
  return (linked ?? ready ?? tabs[0])?.id ?? 'settings';
}

// writeTabToURL makes tab selection refresh-safe and directly linkable while
// preserving any unrelated query parameters.
export function writeTabToURL(tab: string, mode: 'push' | 'replace'): void {
  const url = new URL(window.location.href);
  url.searchParams.set(TAB_PARAM, tab);
  window.history[mode === 'push' ? 'pushState' : 'replaceState']({}, '', url);
}
