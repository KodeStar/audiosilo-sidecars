// Shared timestamp parsing for the pipeline UI. Several call sites parse an
// RFC3339 timestamp and guard against an unparseable value; this centralizes that
// into one helper returning epoch milliseconds or null. Kept React-free and
// unit-tested.

// parseTimestamp parses an RFC3339/ISO timestamp to epoch milliseconds, returning
// null when the input is empty or cannot be parsed.
export function parseTimestamp(ts: string): number | null {
  if (!ts) return null;
  const ms = Date.parse(ts);
  return Number.isNaN(ms) ? null : ms;
}
