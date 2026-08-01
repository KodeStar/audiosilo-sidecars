// Pure duration formatting for the Running tab: a compact elapsed string and a
// rounded ETA string. Kept React-free and unit-tested.

// splitHM splits a whole-minute count into hours + minutes. Callers round to
// whole minutes first, so m is always 0-59 (no "2h 60m" carry to worry about).
function splitHM(totalMinutes: number): { h: number; m: number } {
  return { h: Math.floor(totalMinutes / 60), m: totalMinutes % 60 };
}

// formatDuration renders an elapsed second count compactly: "45s", "5m 20s"
// (dropping "0s"), "2h 10m" (dropping "0m"). Non-finite or negative input is "0s".
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '0s';
  const total = Math.round(seconds);
  if (total < 60) return `${total}s`;
  if (total < 3600) {
    const m = Math.floor(total / 60);
    const s = total % 60;
    return s > 0 ? `${m}m ${s}s` : `${m}m`;
  }
  // For hours, round the leftover to whole minutes.
  const { h, m } = splitHM(Math.round(total / 60));
  return m > 0 ? `${h}h ${m}m` : `${h}h`;
}

// formatEta renders an ETA second count with coarse rounding: "<1m" under a
// minute (also for zero / non-finite / negative), "~40m" for minutes, "~2h 10m"
// (dropping "0m") for hours.
export function formatEta(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 60) return '<1m';
  // Round to whole minutes first, so a value that rounds up to 60 minutes (e.g.
  // 3599s -> 60m) promotes to the hours form rather than rendering "~60m".
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `~${minutes}m`;
  const { h, m } = splitHM(minutes);
  return m > 0 ? `~${h}h ${m}m` : `~${h}h`;
}

// formatWords renders an extracted word count compactly ("142k words"). It is an
// ebook's size chip, standing in for the length chip an audio book gets - an epub
// has no runtime to show.
export function formatWords(words: number): string {
  if (words <= 0) return '';
  if (words < 1000) return `${words} words`;
  if (words < 1_000_000) return `${Math.round(words / 1000)}k words`;
  return `${(words / 1_000_000).toFixed(1)}M words`;
}
