package spelling

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Verdict classifies a roster row against the corrected layer.
type Verdict string

const (
	// VerdictNotNamed - the roster name is never spoken in this book (carryover or role-only).
	VerdictNotNamed Verdict = "not_named"
	// VerdictCarryover - the row claims no chapter (a "books N-M" carryover), and the name is spoken.
	VerdictCarryover Verdict = "carryover"
	// VerdictProblem - the claimed chapter is LATER than the first spoken chapter (a real defect).
	VerdictProblem Verdict = "problem"
	// VerdictAdvisory - the claimed chapter is EARLIER than first spoken (unnamed presence? worth a look).
	VerdictAdvisory Verdict = "advisory"
	// VerdictOK - the claimed chapter equals the first spoken chapter.
	VerdictOK Verdict = "ok"
)

// FirstUseRow is one cross-checked roster row.
type FirstUseRow struct {
	Name        string
	Claimed     int  // the claimed first-appearance chapter; 0 when HasClaim is false
	HasClaim    bool // false for a "books N-M" carryover row (no explicit chapter)
	FirstSpoken int  // the first corrected-layer chapter the name occurs in
	Spoken      bool // false when the name never occurs in the corrected layer
	Verdict     Verdict
}

// FirstUseResult is the outcome of CheckFirstUse.
type FirstUseResult struct {
	Rows     []FirstUseRow
	Problems int
}

// rosterRe parses roster rows like "**Name** ... first appearance: ch12" or a
// "books 2-4" carryover. It ports check_first_use.py's regex, case-insensitive,
// GENERALIZING the Python's literal "books? 1-3" carryover alternative to any
// "books N-M" range. It needs no lookaround, so it uses the stdlib regexp (RE2);
// the non-greedy [^\n]*? spans keep a row bounded to a single line.
var rosterRe = regexp.MustCompile(
	`(?i)\*\*([A-Z][A-Za-z' ]{2,30})\*\*[^\n]*?` +
		`(?:first[- ]appearance|first appears?)[^\n]*?` +
		`(?:ch(?:apter)?\s*(\d+)|books?\s*\d+\s*-\s*\d+)`)

// CheckFirstUse cross-checks every roster first-appearance in the sheet at sheetPath
// against the first corrected-layer chapter the name is actually spoken in. A row
// claiming a chapter LATER than the first spoken occurrence is a PROBLEM (a
// published-position defect); earlier is advisory (unnamed presence); equal is ok.
// Zero parsed rows is an error (the sheet format changed). Ports check_first_use.py.
func CheckFirstUse(workDir, sheetPath string) (*FirstUseResult, error) {
	chapters, sortedCh, err := correctedChapters(workDir)
	if err != nil {
		return nil, fmt.Errorf("read corrected layer: %w", err)
	}
	b, err := os.ReadFile(sheetPath) //nolint:gosec // sheet path is caller-supplied
	if err != nil {
		return nil, fmt.Errorf("read sheet %s: %w", sheetPath, err)
	}
	matches := rosterRe.FindAllStringSubmatch(string(b), -1)
	if len(matches) == 0 {
		return nil, errors.New("no roster rows parsed - check the sheet format")
	}

	res := &FirstUseResult{}
	for _, m := range matches {
		name := strings.TrimSpace(m[1])
		claimed := m[2] // "" when the "books N-M" carryover alternative matched

		firstSpoken, spoken := 0, false
		for _, n := range sortedCh {
			if Occurrences(chapters[n], name) > 0 {
				firstSpoken, spoken = n, true
				break
			}
		}

		row := FirstUseRow{Name: name, FirstSpoken: firstSpoken, Spoken: spoken}
		switch {
		case !spoken:
			row.Verdict = VerdictNotNamed
		case claimed == "":
			row.Verdict = VerdictCarryover
		default:
			c, _ := strconv.Atoi(claimed)
			row.Claimed, row.HasClaim = c, true
			switch {
			case c > firstSpoken:
				row.Verdict = VerdictProblem
				res.Problems++
			case c < firstSpoken:
				row.Verdict = VerdictAdvisory
			default:
				row.Verdict = VerdictOK
			}
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}
