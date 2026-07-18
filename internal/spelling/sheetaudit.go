package spelling

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// This file is the VALIDATOR-side sheet audit, mirroring the deadrules.go precedent:
// it re-derives what the golden-tested engine (GenerateSheets in sheets.go) would
// object to, but EXHAUSTIVELY and with two extra spoiler checks the engine does not
// perform - without touching the frozen engine.
//
// Two motivations:
//
//  1. GenerateSheets/generateSheet fail FAST: the engine returns on the first gate-1
//     or gate-2 violation, so a patch-style validation retry (agent.RunWithBackoff)
//     sees exactly ONE error per attempt and the retry budget drains one message at a
//     time while more violations wait behind it. The exhaustive scan here surfaces
//     EVERY gate-1/gate-2-class violation at once, so one patch pass can fix them all.
//     The messages are copied verbatim from the engine so the feedback is identical.
//
//  2. Two spoiler surfaces are entirely UNGATED by the engine:
//     - a ledger row marked carryover:true rides EVERY sheet from ch0 (generateSheet
//       pins a carryover row's first_use to 0), so a row that is NOT actually a
//       series carryover puts a late-first-heard name on early sheets, and no engine
//       gate scans the row NAME itself;
//     - the preamble is rendered on every sheet, but gate 2 scans only row NOTES, so
//       a preamble line naming a late-first-heard term leaks early.
//     CarryoverErrors and the preamble scan close both.
//
// The first-use computation (newFirstUse) is duplicated from the closure in
// GenerateSheets on purpose: sheets.go is a contract-frozen, golden-tested port and
// must stay byte-for-byte untouched. The duplicate must compute the same value; the
// Python-truthiness quirk (ofu != 0) is preserved exactly, and both are covered by
// their own tests.

// newFirstUse returns a memoized first-use lookup over the corrected chapters,
// identical to the closure GenerateSheets builds: it returns the lowest chapter the
// term is heard in and whether it was heard at all. Not heard -> (0, false).
func newFirstUse(chapters map[int]string, sortedCh []int) func(string) (int, bool) {
	type fuEntry struct {
		chapter int
		heard   bool
	}
	memo := make(map[string]fuEntry)
	return func(term string) (int, bool) {
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
}

// SheetGateErrors walks every sheet the chunk plan would generate and returns EVERY
// gate-1 and gate-2 violation GenerateSheets would raise, instead of stopping at the
// first (Fix A). The messages match the engine's wording verbatim. Returns nil when
// the sheets are gate-clean.
func SheetGateErrors(workDir string, data *Spellings) ([]string, error) {
	chapters, sortedCh, err := correctedChapters(workDir)
	if err != nil {
		return nil, fmt.Errorf("read corrected layer: %w", err)
	}
	return gateErrors(data, newFirstUse(chapters, sortedCh)), nil
}

// gateErrors is the exhaustive gate-1 + gate-2 scan over a shared first-use lookup.
func gateErrors(data *Spellings, fu func(string) (int, bool)) []string {
	var errs []string

	// Gate 1: every non-carryover ledger term must be attested in this book. One
	// aggregate message (matching GenerateSheets, which lists them all with %v).
	var missing []string
	for _, e := range data.Ledger {
		if _, ok := fu(e.Canonical); !e.Carryover && !ok {
			missing = append(missing, e.Canonical)
		}
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Sprintf("gate 1: non-carryover terms with ZERO occurrences in the corrected layer: %v", missing))
	}

	// Gate 2: no included row's note may name another ledger canonical first heard
	// AFTER the sheet's chapter. Deduped per (row, named term) at the SMALLEST sheet
	// end it violates (ChunkEnds is strictly increasing, so ascending iteration hits
	// the smallest first) - that is exactly the end the fail-fast engine would report,
	// so the message stays identical while every distinct leaking pair is surfaced.
	type pair struct{ row, other string }
	seen := make(map[pair]bool)
	for _, end := range data.ChunkEnds {
		for _, e := range data.Ledger {
			f := 0
			if !e.Carryover {
				v, ok := fu(e.Canonical)
				if !ok { // not heard yet - not on this sheet
					continue
				}
				f = v
			}
			if f > end || e.Note == "" {
				continue
			}
			for _, other := range data.Ledger {
				if other.Canonical == e.Canonical || !strings.Contains(e.Note, other.Canonical) {
					continue
				}
				// Python: `if ofu and ofu > end` - the truthiness makes a first_use of 0
				// (a ch0 hit) falsy, so ofu != 0 replicates the engine exactly.
				ofu, ok := fu(other.Canonical)
				if !ok || ofu == 0 || ofu <= end {
					continue
				}
				p := pair{e.Canonical, other.Canonical}
				if seen[p] {
					continue
				}
				seen[p] = true
				errs = append(errs, fmt.Sprintf("gate 2: sheet ch%d: row %q names %q, first heard ch%d",
					end, e.Canonical, other.Canonical, ofu))
			}
		}
	}
	return errs
}

// CarryoverErrors flags each ledger row marked carryover:true whose canonical is not
// present in the staged prior-book ledger (Fix B, part 1). A carryover row rides
// every sheet from ch0, so a row that is not a real series carryover leaks a
// late-first-heard name onto early sheets - and no engine gate scans the row name.
// When no prior ledger was staged (hasPrior false - a series opener, or the
// predecessor never produced a ledger), NO row may be a carryover.
//
// The check is pure (no I/O): priorCanonical is the set of the predecessor ledger's
// canonicals (see PriorCanonicals). Rationale for strictness: carryover's ONLY effect
// is showing a row from the first sheet; first_use gating is always spoiler-safe, so
// when in doubt carryover:false loses nothing a reader would miss.
func CarryoverErrors(data *Spellings, priorCanonical map[string]bool, hasPrior bool) []string {
	var errs []string
	for _, e := range data.Ledger {
		if !e.Carryover {
			continue
		}
		c := strings.TrimSpace(e.Canonical)
		switch {
		case !hasPrior:
			errs = append(errs, fmt.Sprintf("carryover: row %q is marked carryover:true but no prior-book ledger was staged for this book - a carryover must appear in the predecessor's ledger; set carryover:false (first_use gating still shows it from the right sheet, so nothing is lost)", c))
		case !priorCanonical[c]:
			errs = append(errs, fmt.Sprintf("carryover: row %q is marked carryover:true but its canonical is not in the staged prior-book ledger (spelling-refs/prior-spellings.json) - set carryover:false (first_use gating shows it from the right sheet, so nothing is lost)", c))
		}
	}
	return errs
}

// preambleLeaks flags a preamble line that names a ledger canonical first heard after
// the FIRST chunk end (Fix B, part 2). The preamble renders on every sheet - the
// first sheet is the tightest gate - but gate 2 scans only row notes, so this surface
// is otherwise ungated. It mirrors gate 2's predicate, including the ofu != 0
// Python-truthiness quirk.
func preambleLeaks(data *Spellings, fu func(string) (int, bool)) []string {
	if len(data.ChunkEnds) == 0 {
		return nil
	}
	firstEnd := data.ChunkEnds[0]
	var errs []string
	for i, line := range data.Preamble {
		for _, e := range data.Ledger {
			canon := strings.TrimSpace(e.Canonical)
			if canon == "" || !strings.Contains(line, e.Canonical) {
				continue
			}
			ofu, ok := fu(e.Canonical)
			if !ok || ofu == 0 || ofu <= firstEnd {
				continue
			}
			errs = append(errs, fmt.Sprintf("preamble: line %d names %q, first heard ch%d (after the first sheet ch%d) - the preamble renders on EVERY sheet; defer the detail without naming the term",
				i+1, e.Canonical, ofu, firstEnd))
		}
	}
	return errs
}

// AuditSheets runs every validator-side sheet check and returns EVERY violation
// across all of them (Fix A + Fix B), so one patch retry can fix them all at once:
// the exhaustive gate-1/gate-2 scan, carryover integrity, and preamble safety.
// priorCanonical / hasPrior describe the staged prior-book ledger (see
// PriorCanonicals). Returns nil when the output is clean.
func AuditSheets(workDir string, data *Spellings, priorCanonical map[string]bool, hasPrior bool) ([]string, error) {
	chapters, sortedCh, err := correctedChapters(workDir)
	if err != nil {
		return nil, fmt.Errorf("read corrected layer: %w", err)
	}
	fu := newFirstUse(chapters, sortedCh)
	var errs []string
	errs = append(errs, gateErrors(data, fu)...)
	errs = append(errs, CarryoverErrors(data, priorCanonical, hasPrior)...)
	errs = append(errs, preambleLeaks(data, fu)...)
	return errs, nil
}

// PriorCanonicals reads a staged predecessor spellings ledger at path and returns the
// set of its ledger canonicals plus whether the file was present. A missing file is
// (nil, false, nil) - the no-predecessor case, where CarryoverErrors rejects every
// carryover. The file is parsed leniently (no Validate) so a predecessor quirk cannot
// break this book's validation; only the ledger canonicals are extracted.
func PriorCanonicals(path string) (map[string]bool, bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path derives from the book's staged work dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var s Spellings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, false, fmt.Errorf("parse prior ledger %s: %w", path, err)
	}
	set := make(map[string]bool, len(s.Ledger))
	for _, e := range s.Ledger {
		if c := strings.TrimSpace(e.Canonical); c != "" {
			set[c] = true
		}
	}
	return set, true, nil
}
