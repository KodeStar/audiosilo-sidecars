// Resolves the API base URL at app boot.
//
// Order:
//   1. Fetch same-origin `/config.json`. If it returns 200 with JSON
//      `{"api_base":"https://host:8090"}`, use that api_base (trailing slash trimmed).
//   2. Otherwise (404, network error, or a missing/empty api_base) fall back to
//      same-origin: an empty base string, so requests hit `/api/v1/...` on the
//      current origin.
//
// This lets the SPA be deployed separately (Docker/nginx) pointed at a remote
// daemon, while the daemon's own embedded build works same-origin.

// Pure resolver, unit-tested. Accepts whatever `/config.json` parsed to.
export function resolveApiBase(configJson: unknown): string {
  if (typeof configJson === 'object' && configJson !== null && 'api_base' in configJson) {
    const value = (configJson as { api_base?: unknown }).api_base;
    if (typeof value === 'string' && value.trim() !== '') {
      return value.trim().replace(/\/+$/, '');
    }
  }
  return '';
}

export async function fetchApiBase(): Promise<string> {
  try {
    const res = await fetch('/config.json', { cache: 'no-store' });
    if (!res.ok) {
      return '';
    }
    const json: unknown = await res.json();
    return resolveApiBase(json);
  } catch {
    return '';
  }
}
