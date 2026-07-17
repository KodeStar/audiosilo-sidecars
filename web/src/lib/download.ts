// Helpers for downloading a fetched Blob (the Done tab's sidecars export zip). The
// filename parse is pure and unit-tested; triggerBlobDownload does the DOM side
// effect (object URL + a synthetic anchor click) and is not unit-tested.

// parseContentDispositionFilename extracts the filename from a Content-Disposition
// header, falling back to the given name when the header is missing or has no
// filename. It handles both `filename="x.zip"` and the RFC 5987 `filename*=UTF-8''x`
// form, and strips surrounding quotes. The fallback is used verbatim (never empty).
export function parseContentDispositionFilename(header: string | null, fallback: string): string {
  if (!header) return fallback;

  // Prefer the RFC 5987 extended form when present (filename*=UTF-8''<pct-encoded>).
  const ext = /filename\*=(?:UTF-8'')?([^;]+)/i.exec(header);
  if (ext) {
    const raw = ext[1].trim().replace(/^"|"$/g, '');
    try {
      const decoded = decodeURIComponent(raw);
      if (decoded) return decoded;
    } catch {
      if (raw) return raw;
    }
  }

  const plain = /filename="?([^";]+)"?/i.exec(header);
  if (plain) {
    const name = plain[1].trim();
    if (name) return name;
  }
  return fallback;
}

// triggerBlobDownload saves a Blob to the user's downloads via a transient object
// URL and a synthetic anchor click, then revokes the URL. Browser-only. The revoke
// is DEFERRED: revoking synchronously right after click() can abort the still-async
// download on Firefox/Safari, so we let the browser start the save first.
const REVOKE_DELAY_MS = 10_000;

export function triggerBlobDownload(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url), REVOKE_DELAY_MS);
}
