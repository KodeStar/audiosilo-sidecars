package spelling

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Fix A: exhaustive gate scan ------------------------------------------

// SheetGateErrors returns EVERY gate-1 and gate-2 violation in ONE call, where the
// frozen engine (GenerateSheets) fails fast on the first. This proves the retry loop
// can fix them all at once instead of one message per attempt.
func TestSheetGateErrorsExhaustive(t *testing.T) {
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{
		1: "Rowan appears.",
		2: "Marcus and Sarah appear.",
		8: "Voidmind and Doom revealed.",
	})
	data := &Spellings{
		Title:     "T",
		ChunkEnds: []int{2, 10},
		Ledger: []LedgerEntry{
			{Canonical: "Rowan", Carryover: true, Note: "later becomes Voidmind"},
			{Canonical: "Marcus", Carryover: false, Note: "allied with Doom"},
			{Canonical: "Voidmind", Carryover: false, Note: "-"},
			{Canonical: "Doom", Carryover: false, Note: "-"},
			{Canonical: "Ghost", Carryover: false, Note: "-"}, // never heard -> gate 1
		},
	}
	errs, err := SheetGateErrors(work, data)
	if err != nil {
		t.Fatal(err)
	}
	// Expect three distinct violations in one call: gate 1 (Ghost), gate 2 Rowan->Voidmind
	// and gate 2 Marcus->Doom, each at the smallest triggering sheet (ch2).
	if len(errs) != 3 {
		t.Fatalf("want 3 violations, got %d: %v", len(errs), errs)
	}
	joined := strings.Join(errs, "\n")
	for _, want := range []string{
		`gate 1: non-carryover terms with ZERO occurrences in the corrected layer: [Ghost]`,
		`gate 2: sheet ch2: row "Rowan" names "Voidmind", first heard ch8`,
		`gate 2: sheet ch2: row "Marcus" names "Doom", first heard ch8`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing violation %q in:\n%s", want, joined)
		}
	}
}

// The engine's fail-fast gate is faithfully reproduced: the same fixture that trips
// SheetGateErrors also trips GenerateSheets (so the exhaustive scan never disagrees
// on WHETHER the output is clean, only on how many messages it emits).
func TestSheetGateErrorsAgreesWithEngine(t *testing.T) {
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{1: "Early here.", 15: "Late here."})
	data := &Spellings{
		Title:     "T",
		ChunkEnds: []int{8},
		Ledger: []LedgerEntry{
			{Canonical: "Early", Carryover: true, Note: "related to Late"},
			{Canonical: "Late", Carryover: false, Note: "-"},
		},
	}
	errs, err := SheetGateErrors(work, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "first heard ch15") {
		t.Fatalf("want one gate-2 error naming ch15, got %v", errs)
	}
	if _, gerr := GenerateSheets(work, data); gerr == nil {
		t.Fatal("the engine should also reject this fixture")
	}
}

// --- Fix B1: carryover integrity (pure) -----------------------------------

func TestCarryoverErrors(t *testing.T) {
	data := &Spellings{
		Ledger: []LedgerEntry{
			{Canonical: "Known", Carryover: true},
			{Canonical: "Unknown", Carryover: true},
			{Canonical: "Fresh", Carryover: false}, // not a carryover -> never flagged
		},
	}

	// With a prior ledger that has only "Known": "Unknown" is flagged, "Known" is not.
	prior := map[string]bool{"Known": true}
	errs := CarryoverErrors(data, prior, true)
	if len(errs) != 1 || !strings.Contains(errs[0], `"Unknown"`) {
		t.Fatalf("want one error naming Unknown, got %v", errs)
	}
	if strings.Contains(strings.Join(errs, "\n"), `"Known"`) {
		t.Errorf("Known is in the prior ledger and must not be flagged: %v", errs)
	}

	// No prior ledger staged: EVERY carryover row is flagged.
	errs = CarryoverErrors(data, nil, false)
	if len(errs) != 2 {
		t.Fatalf("no-prior case: want 2 errors (both carryovers), got %d: %v", len(errs), errs)
	}
	for _, e := range errs {
		if !strings.Contains(e, "no prior-book ledger was staged") {
			t.Errorf("no-prior message should name the missing ledger: %q", e)
		}
	}
}

// --- Fix B2: preamble safety (via the aggregator) -------------------------

func TestAuditSheetsPreambleLeak(t *testing.T) {
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{1: "Hero acts.", 8: "Villain revealed."})
	data := &Spellings{
		Title:     "T",
		ChunkEnds: []int{2},
		Preamble:  []string{"This book follows Hero against Villain."},
		Ledger: []LedgerEntry{
			{Canonical: "Hero", Carryover: false, Note: "-"},
			{Canonical: "Villain", Carryover: false, Note: "-"},
		},
	}
	errs, err := AuditSheets(work, data, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	// Only the preamble check fires: Villain (ch8) named before the first sheet (ch2).
	// Hero (ch1) is safe; the ledger is otherwise gate-clean.
	if len(errs) != 1 {
		t.Fatalf("want exactly the preamble violation, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0], "preamble") || !strings.Contains(errs[0], `"Villain"`) || !strings.Contains(errs[0], "ch8") {
		t.Errorf("preamble error = %q, want it to name Villain / ch8", errs[0])
	}
}

// --- A clean output passes every check ------------------------------------

func TestAuditSheetsClean(t *testing.T) {
	work := t.TempDir()
	writeCorrected(t, work, map[int]string{1: "Alpha here.", 2: "Beta here."})
	data := &Spellings{
		Title:     "T",
		ChunkEnds: []int{2},
		Preamble:  []string{"A neutral opening note."},
		Ledger: []LedgerEntry{
			{Canonical: "Alpha", Carryover: true, Note: "known from before"},
			{Canonical: "Beta", Carryover: false, Note: "as-heard"},
		},
	}
	errs, err := AuditSheets(work, data, map[string]bool{"Alpha": true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 0 {
		t.Fatalf("clean output should produce no violations, got: %v", errs)
	}
}

// --- PriorCanonicals: presence + parse ------------------------------------

func TestPriorCanonicals(t *testing.T) {
	// Missing file -> (nil, false, nil): the no-predecessor case.
	set, present, err := PriorCanonicals(filepath.Join(t.TempDir(), "prior-spellings.json"))
	if err != nil || present || set != nil {
		t.Fatalf("missing file: got set=%v present=%v err=%v", set, present, err)
	}

	// A present ledger -> its canonical set + present=true.
	dir := t.TempDir()
	p := filepath.Join(dir, "prior-spellings.json")
	body := `{"title":"Prior","chunk_ends":[5],"ledger":[{"canonical":"Celaine"},{"canonical":"Voidmind"}]}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	set, present, err = PriorCanonicals(p)
	if err != nil || !present {
		t.Fatalf("present file: present=%v err=%v", present, err)
	}
	if !set["Celaine"] || !set["Voidmind"] || len(set) != 2 {
		t.Errorf("prior canonicals = %v, want {Celaine, Voidmind}", set)
	}
}
