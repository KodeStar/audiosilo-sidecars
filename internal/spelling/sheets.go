package spelling

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
)

// Fixed sheet template strings (engine, identical for every book). The book-specific
// prose is the Spellings.Preamble; these headings/placeholders are the engine's.
const (
	tableHeaderCols = "| canonical | type | status | first_use | ASR variants seen | note |"
	tableHeaderSep  = "| --- | --- | --- | --- | --- | --- |"
	unresolvedHead  = "## Unresolved / do-not-publish-clean (heard but ambiguous or invented)"
	unresolvedNote  = "NEVER publish these as clean names. Describe the figure BY ROLE or surname, or omit."
	unresolvedNone  = "(none heard yet)"
	clustersHead    = "## DO-NOT-MERGE clusters (look-alike ASR strings that are DISTINCT entities)"
	clustersNone    = "(none applicable yet)"
	nonMergesHead   = "## Deliberate non-merges (carryover pairs the ASR blurs)"
)

// SheetStat reports one generated sheet.
type SheetStat struct {
	Chapter    int
	Path       string
	Rows       int
	Unresolved int
	Clusters   int
}

// SheetsResult reports every generated sheet.
type SheetsResult struct {
	Sheets []SheetStat
}

// GenerateSheets writes facts/spellings-through-ch<N>.md for each chunk end,
// porting generate_spellings.py. Each sheet lists only terms whose FIRST heard use
// is at or before chapter N, so a later-chapter name never leaks across the spoiler
// boundary. first_use is computed mechanically from the corrected layer; carryover
// rows are pinned to first_use 0.
//
// Two gates fire on real defects:
//
//	Gate 1: every non-carryover ledger canonical must have a first_use (else it is a
//	        rule/marker-only name the ASR never produced - error).
//	Gate 2: no included row's note may name another ledger canonical first heard
//	        AFTER the sheet's chapter (the Book 3 spoiler-leak defect - error).
//
// A gate failure returns a nil result and an error (no sheets are written for that run).
func GenerateSheets(workDir string, data *Spellings) (*SheetsResult, error) {
	chapters, sortedCh, err := correctedChapters(workDir)
	if err != nil {
		return nil, fmt.Errorf("read corrected layer: %w", err)
	}

	// fu memoizes first_use per term. Because first_use is a deterministic function
	// of the corrected layer, the Python's setdefault layering of first_use / aux_fu /
	// fu_all (ledger, then unresolved, then cluster names) is a no-op - every path
	// computes the same lowest-chapter-heard - so one memoized lookup is faithful.
	type fuEntry struct {
		chapter int
		heard   bool
	}
	memo := make(map[string]fuEntry)
	fu := func(term string) (int, bool) {
		if e, ok := memo[term]; ok {
			return e.chapter, e.heard
		}
		for _, n := range sortedCh {
			if Occurrences(chapters[n], term) > 0 {
				memo[term] = fuEntry{chapter: n, heard: true}
				return n, true
			}
		}
		memo[term] = fuEntry{}
		return 0, false
	}

	// Gate 1: every non-carryover ledger term must be attested in this book.
	var missing []string
	for _, e := range data.Ledger {
		if _, ok := fu(e.Canonical); !e.Carryover && !ok {
			missing = append(missing, e.Canonical)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("gate 1: non-carryover terms with ZERO occurrences in the corrected layer: %v", missing)
	}

	result := &SheetsResult{}
	for _, end := range data.ChunkEnds {
		stat, err := generateSheet(workDir, data, end, fu)
		if err != nil {
			return nil, err
		}
		result.Sheets = append(result.Sheets, stat)
	}
	return result, nil
}

// sheetRow is a ledger entry resolved to its display first_use for a given chunk.
type sheetRow struct {
	entry LedgerEntry
	fu    int
}

func generateSheet(workDir string, data *Spellings, end int, fu func(string) (int, bool)) (SheetStat, error) {
	var rows []sheetRow
	for _, e := range data.Ledger {
		f := 0
		if !e.Carryover {
			v, ok := fu(e.Canonical)
			if !ok { // first_use is None - not heard yet
				continue
			}
			f = v
		}
		if f > end {
			continue
		}
		// Gate 2: a row's note must not name a term first heard AFTER this sheet.
		for _, other := range data.Ledger {
			if other.Canonical == e.Canonical || e.Note == "" || !strings.Contains(e.Note, other.Canonical) {
				continue
			}
			// Python: `if ofu and ofu > end` - the truthiness makes a first_use of 0
			// (a ch0 hit) falsy, so it never triggers; ofu != 0 replicates that.
			ofu, ok := fu(other.Canonical)
			if ok && ofu != 0 && ofu > end {
				return SheetStat{}, fmt.Errorf("gate 2: sheet ch%d: row %q names %q, first heard ch%d",
					end, e.Canonical, other.Canonical, ofu)
			}
		}
		rows = append(rows, sheetRow{entry: e, fu: f})
	}

	lines := []string{fmt.Sprintf("# %s - through chapter %d", data.Title, end), ""}
	lines = append(lines, data.Preamble...)
	lines = append(lines, "", tableHeaderCols, tableHeaderSep)
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("| **%s** | %s | %s | ch%d | %s | %s |",
			r.entry.Canonical, r.entry.Type, r.entry.Status, r.fu, r.entry.Variants, r.entry.Note))
	}

	// Unresolved - gated by their own first_use so a late name never leaks early.
	var unresHere []string
	for _, n := range data.Unresolved {
		if f, ok := fu(n); ok && f <= end {
			unresHere = append(unresHere, n)
		}
	}
	unresLine := unresolvedNone
	if len(unresHere) > 0 {
		unresLine = strings.Join(unresHere, ", ")
	}
	lines = append(lines,
		"", unresolvedHead,
		"", unresolvedNote,
		"", "  "+unresLine,
		"", clustersHead, "",
	)

	// Clusters - shown only when ALL referenced names have been heard by this chunk.
	var clusterLines []string
	for _, cl := range data.Clusters {
		all := true
		for _, n := range cl.Names {
			f, ok := fu(n)
			if !ok || f > end {
				all = false
				break
			}
		}
		if all {
			clusterLines = append(clusterLines, cl.Text)
		}
	}
	if len(clusterLines) > 0 {
		lines = append(lines, clusterLines...)
	} else {
		lines = append(lines, clustersNone)
	}

	lines = append(lines, "", nonMergesHead, "")

	// Non-merges - shown when both names are present ledger canonicals within the
	// window. Python: present = { c : first_use[c] is not None and (first_use[c] or 0)
	// <= end }; the `or 0` is a no-op for a non-None int, so it reduces to f <= end.
	present := make(map[string]bool)
	for _, e := range data.Ledger {
		if f, ok := fu(e.Canonical); ok && f <= end {
			present[e.Canonical] = true
		}
	}
	for _, nm := range data.NonMerges {
		if present[nm.A] && present[nm.B] {
			lines = append(lines, nm.Text)
		}
	}

	content := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(workDir, FactsDir, fmt.Sprintf("spellings-through-ch%d.md", end))
	if err := fsutil.WriteFileAtomic(path, []byte(content), 0o644); err != nil {
		return SheetStat{}, fmt.Errorf("write %s: %w", path, err)
	}
	return SheetStat{Chapter: end, Path: path, Rows: len(rows), Unresolved: len(unresHere), Clusters: len(clusterLines)}, nil
}
