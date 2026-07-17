import type { ScanProgress } from '../api/types';

// runningScanDetail builds the status line shown while a scan is running (the
// caller appends a trailing "..."). Three phases:
//   - coverage: "Checking coverage 3/12"
//   - scanning, tree enumerated (groups_total > 0): "Scanning folders 3/12 - 5 books found"
//   - scanning, walk still enumerating (groups_total === 0): live walk counts,
//     "Scanning folders - 42 folders, 5 books found", falling back to bare
//     "Scanning folders" until the first directory is counted.
export function runningScanDetail(p: ScanProgress): string {
  if (p.phase === 'coverage') {
    return p.coverage_total > 0
      ? `Checking coverage ${p.coverage_done}/${p.coverage_total}`
      : 'Checking coverage';
  }
  if (p.groups_total > 0) {
    const found =
      p.books_found > 0 ? ` - ${p.books_found} book${p.books_found === 1 ? '' : 's'} found` : '';
    return `Scanning folders ${p.groups_done}/${p.groups_total}${found}`;
  }
  if (p.walk_dirs > 0) {
    const dirs = `${p.walk_dirs} folder${p.walk_dirs === 1 ? '' : 's'}`;
    const groups = `${p.walk_groups} book${p.walk_groups === 1 ? '' : 's'} found`;
    return `Scanning folders - ${dirs}, ${groups}`;
  }
  return 'Scanning folders';
}
