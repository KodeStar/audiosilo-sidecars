// Shared numeric-input parsing for the settings form mappers (agentSettings /
// contributionSettings). Kept React-free and unit-tested.

// parseIntOrNaN parses a base-10 integer, returning NaN for blank/non-numeric input
// (so validation rejects it) rather than parseInt's lenient prefix parse.
export function parseIntOrNaN(raw: string): number {
  const s = raw.trim();
  if (s === '' || !/^-?\d+$/.test(s)) return NaN;
  return Number(s);
}

// parseFloatOrNaN parses a decimal number, returning NaN for blank/non-numeric input
// (so validation rejects it) rather than parseFloat's lenient prefix parse. Accepts an
// optional single decimal point (e.g. "75", "120.5").
export function parseFloatOrNaN(raw: string): number {
  const s = raw.trim();
  if (s === '' || !/^-?\d+(\.\d+)?$/.test(s)) return NaN;
  return Number(s);
}
