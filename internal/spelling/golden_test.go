// Golden tests that replay the REAL historical HW05 extraction work dir against the
// Go spelling engines and compare their output to the Python originals' on-disk
// output.
//
// # Env gate + read-only guarantee
//
// These tests run only when AUDIOSILO_EXTRACTION_DIR points at a local checkout of
// the historical extraction work dirs AND the HW05 book dir exists; otherwise they
// t.Skip cleanly (CI and any machine without the private data skip). They also skip
// when python3 is not on PATH (the per-book data is exported at runtime by
// scripts/export-extraction-data.py). The extraction dir is treated as strictly
// READ-ONLY: the engine runs happen in a t.TempDir() copy, and the source layers are
// only ever read.
//
// # Why the in-repo expectations are numbers-only
//
// The hard contract: no transcript text and no character/place NAME (Owalyn, Daston,
// ...) may enter the repo. So this file hard-codes only NUMBERS and structure - rule
// and chapter counts, chunk boundaries. The per-book DATA (rules, ledger, clusters)
// is exported from the original Python at runtime by the committed exporter and never
// hand-copied; every name/text comparison happens against the locally-parsed
// historical files. Nothing about the books' prose or cast is committed here.
package spelling

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

const extractionEnv = "AUDIOSILO_EXTRACTION_DIR"

// Path fragments under the extraction dir (never book text).
const (
	hw05Rel          = "hedge-wizard/work5"
	work4SpellingRel = "hedge-wizard/work4/spelling-source"
)

// goldenSetup gates the env/python3/book preconditions, exports the HW05 data via the
// committed Python exporter, builds a temp work dir (transcripts-text/,
// transcripts-repaired/, marker_titles.txt, plus the exported corrections.json /
// spellings.json), and returns the temp work dir and the read-only source dir.
func goldenSetup(t *testing.T) (workDir, srcDir string) {
	t.Helper()
	root := os.Getenv(extractionEnv)
	if root == "" {
		t.Skipf("%s not set - skipping golden replay (needs the local historical extraction data)", extractionEnv)
	}
	srcDir = filepath.Join(root, filepath.FromSlash(hw05Rel))
	if _, err := os.Stat(filepath.Join(srcDir, "apply_corrections.py")); err != nil {
		t.Skipf("%s missing apply_corrections.py (%v) - skipping", hw05Rel, err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not on PATH (%v) - skipping (the exporter needs it)", err)
	}

	workDir = t.TempDir()

	// Export the per-book data tables into the temp WORK dir (where LoadCorrections /
	// LoadSpellings read them from).
	script := filepath.Join(repoRoot(t), "scripts", "export-extraction-data.py")
	cmd := exec.Command("python3", script, srcDir, workDir) //nolint:gosec // fixed script + test-controlled dirs
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("export-extraction-data.py: %v\n%s", err, out)
	}

	// Copy the source layers the engines read.
	copyDirFiles(t, filepath.Join(srcDir, transcript.TextDir), filepath.Join(workDir, transcript.TextDir))
	if rep := filepath.Join(srcDir, transcript.RepairedDir); dirExists(rep) {
		copyDirFiles(t, rep, filepath.Join(workDir, transcript.RepairedDir))
	}
	copyFileGolden(t, filepath.Join(srcDir, markerTitlesName), filepath.Join(workDir, markerTitlesName))
	return workDir, srcDir
}

// markerTitlesName is the historical tier-1 chapter-marker-titles file. It is not an
// engine constant: attestation sources are purely data (Corrections.ReferenceFiles),
// so the golden test names the file itself and passes it explicitly.
const markerTitlesName = "marker_titles.txt"

// TestGoldenSpellingApply: Apply produces byte-for-byte the historical corrected
// layer, and its per-rule replacement counts equal the historical corrections.log.
func TestGoldenSpellingApply(t *testing.T) {
	workDir, srcDir := goldenSetup(t)

	data, err := LoadCorrections(workDir)
	if err != nil {
		t.Fatalf("LoadCorrections: %v", err)
	}
	res, err := Apply(workDir, data)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Byte-compare every generated corrected chapter against the historical one.
	srcCorrected := filepath.Join(srcDir, CorrectedDir)
	entries, err := os.ReadDir(srcCorrected)
	if err != nil {
		t.Fatalf("read historical %s: %v", CorrectedDir, err)
	}
	compared := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "ch") || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		want := readFileGolden(t, filepath.Join(srcCorrected, e.Name()))
		gotPath := filepath.Join(workDir, CorrectedDir, e.Name())
		got, err := os.ReadFile(gotPath) //nolint:gosec // test-controlled temp path
		if err != nil {
			t.Fatalf("read generated %s: %v", e.Name(), err)
		}
		if string(got) != string(want) {
			t.Fatalf("corrected %s differs from historical:\n%s", e.Name(), firstLineDiff(string(want), string(got)))
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("no corrected chapters compared")
	}
	t.Logf("byte-identical corrected layer: %d chapters", compared)

	// Per-rule counts must equal the historical corrections.log (matched by rule
	// INDEX, since our log renders $1 where the historical renders \1).
	wantCounts := parseLogRuleCounts(t, readFileGolden(t, filepath.Join(srcDir, "corrections.log")))
	if len(wantCounts) != len(res.Rules) {
		t.Fatalf("rule count: historical log %d, Apply %d", len(wantCounts), len(res.Rules))
	}
	for i, rf := range res.Rules {
		if rf.Count != wantCounts[i] {
			t.Errorf("rule %d count: Apply %d, historical log %d", i, rf.Count, wantCounts[i])
		}
	}
}

// TestGoldenSpellingCheck: Check over the corrected layer passes every gate, with the
// Book 4 mirror as the gate-3 reference source (the historical book passed its own
// check_corrections.py).
func TestGoldenSpellingCheck(t *testing.T) {
	workDir, srcDir := goldenSetup(t)

	// Rebuild the corrected layer Check reads.
	data, err := LoadCorrections(workDir)
	if err != nil {
		t.Fatalf("LoadCorrections: %v", err)
	}
	if _, err := Apply(workDir, data); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	mirror := filepath.Join(filepath.Dir(srcDir), "work4", "spelling-source")
	if !dirExists(mirror) {
		t.Skipf("%s absent - skipping gate-3 strictness (no Book 4 mirror)", work4SpellingRel)
	}
	// The attestation sources the historical check_corrections.py unioned: the Book 4
	// mirror plus the tier-1 marker titles - both explicit entries now, since the
	// engine consults nothing implicitly.
	data.ReferenceFiles = []string{mirror, markerTitlesName}

	res, err := Check(workDir, data)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Ok() {
		t.Fatalf("Check failed (%d failures):\n%s", len(res.Failures), res.Summary())
	}
	t.Logf("Check OK: %d rules, %d layer words", res.RulesChecked, res.LayerWords)
}

// TestGoldenSpellingSheets: GenerateSheets produces, for each historical chunk, the
// identical table rows (name + first_use, in order), identical unresolved names, and
// the same cluster-warning count. Not a byte compare (the header/preamble prose is
// data-driven and not exported).
func TestGoldenSpellingSheets(t *testing.T) {
	workDir, srcDir := goldenSetup(t)

	corr, err := LoadCorrections(workDir)
	if err != nil {
		t.Fatalf("LoadCorrections: %v", err)
	}
	if _, err := Apply(workDir, corr); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	spell, err := LoadSpellings(workDir)
	if err != nil {
		t.Fatalf("LoadSpellings: %v", err)
	}
	if _, err := GenerateSheets(workDir, spell); err != nil {
		t.Fatalf("GenerateSheets: %v", err)
	}

	for _, end := range spell.ChunkEnds {
		fname := "spellings-through-ch" + strconv.Itoa(end) + ".md"
		wantSheet := parseSheet(string(readFileGolden(t, filepath.Join(srcDir, FactsDir, fname))))
		gotSheet := parseSheet(string(readFileGolden(t, filepath.Join(workDir, FactsDir, fname))))

		if !slices.Equal(gotSheet.rows, wantSheet.rows) {
			t.Errorf("ch%d sheet rows differ:\n  historical: %v\n  generated:  %v", end, wantSheet.rows, gotSheet.rows)
		}
		if !slices.Equal(gotSheet.unresolved, wantSheet.unresolved) {
			t.Errorf("ch%d unresolved differ:\n  historical: %v\n  generated:  %v", end, wantSheet.unresolved, gotSheet.unresolved)
		}
		if gotSheet.clusterCount != wantSheet.clusterCount {
			t.Errorf("ch%d cluster count: historical %d, generated %d", end, wantSheet.clusterCount, gotSheet.clusterCount)
		}
	}
}

// TestGoldenSpellingFirstUse (best-effort): HW05's knowledge-final.md uses a
// narrative roster format that NEITHER the Python check_first_use.py NOR the Go
// parser recognizes - the Python `sys.exit("no ROSTER rows parsed ...")` before
// printing any table, and the Go CheckFirstUse returns its own "no roster rows
// parsed" error. So the golden here is that the two AGREE that zero rows parse (0
// rows checked, 0 problems), rather than comparing a table the Python never produced.
func TestGoldenSpellingFirstUse(t *testing.T) {
	workDir, srcDir := goldenSetup(t)

	corr, err := LoadCorrections(workDir)
	if err != nil {
		t.Fatalf("LoadCorrections: %v", err)
	}
	if _, err := Apply(workDir, corr); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	sheetPath := filepath.Join(srcDir, FactsDir, "knowledge-final.md")
	if _, statErr := os.Stat(sheetPath); statErr != nil {
		t.Skipf("no knowledge-final.md (%v) - skipping first-use golden", statErr)
	}

	_, err = CheckFirstUse(workDir, sheetPath)
	if err == nil {
		t.Fatalf("CheckFirstUse parsed rows from knowledge-final.md, but the historical " +
			"check_first_use.py parses zero (narrative roster format); expected a no-rows error")
	}
	if !strings.Contains(err.Error(), "no roster rows parsed") {
		t.Fatalf("unexpected CheckFirstUse error: %v", err)
	}
	t.Log("CheckFirstUse agrees with the historical Python: knowledge-final.md yields 0 parseable roster rows (0 rows, 0 problems)")
}

// --- corrections.log parsing (counts only) ----------------------------------

// logRuleLineRe extracts a rule's replacement count from a corrections.log line. The
// "  x<N>  # " separator between the replacement and the note is unambiguous: a rule
// line is "- <pattern> -> <repl>  x<count>  # <note>", and neither pattern nor
// replacement contains that separator (a note may contain a bare "x69", but never the
// two-space-hash-space form). The first match per line is the count.
var logRuleLineRe = regexp.MustCompile(`  x(\d+)  # `)

// parseLogRuleCounts returns the per-rule replacement counts in file order from the
// rule block of a corrections.log (the block between the header's blank line and the
// "## per chapter" section).
func parseLogRuleCounts(t *testing.T, log []byte) []int {
	t.Helper()
	var counts []int
	for _, ln := range strings.Split(string(log), "\n") {
		if ln == "## per chapter" {
			break
		}
		if !strings.HasPrefix(ln, "- ") {
			continue
		}
		m := logRuleLineRe.FindStringSubmatch(ln)
		if m == nil {
			t.Fatalf("corrections.log rule line has no count: %q", ln)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parse count %q: %v", m[1], err)
		}
		counts = append(counts, n)
	}
	if len(counts) == 0 {
		t.Fatal("no rule counts parsed from corrections.log")
	}
	return counts
}

// --- sheet parsing (rows/unresolved/clusters, structure only) ---------------

type goldenRow struct {
	name     string
	firstUse int
}

type parsedSheet struct {
	rows         []goldenRow
	unresolved   []string
	clusterCount int
}

var sheetRowRe = regexp.MustCompile(`^\| \*\*(.+?)\*\* \|`)

// parseSheet extracts the numeric/name structure of a generated spelling sheet: the
// table rows (canonical name + first_use chapter, in order), the unresolved names
// line, and the count of DO-NOT-MERGE cluster warnings shown. It parses both the
// historical and generated sheets the same way; the data-driven header/preamble prose
// is ignored (not byte-compared).
func parseSheet(md string) parsedSheet {
	lines := strings.Split(md, "\n")
	var ps parsedSheet
	for _, ln := range lines {
		m := sheetRowRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		cols := strings.Split(ln, "|") // ["", " **Name** ", " type ", " status ", " chF ", ...]
		if len(cols) < 5 {
			continue
		}
		fu := strings.TrimSpace(cols[4])
		fu = strings.TrimPrefix(fu, "ch")
		n, _ := strconv.Atoi(fu)
		ps.rows = append(ps.rows, goldenRow{name: strings.TrimSpace(m[1]), firstUse: n})
	}
	ps.unresolved = parseUnresolved(lines)
	ps.clusterCount = countClusters(lines)
	return ps
}

// parseUnresolved returns the names on the unresolved line (the indented line after
// the "NEVER publish these ..." note), or nil for "(none heard yet)".
func parseUnresolved(lines []string) []string {
	const marker = "NEVER publish these"
	for i, ln := range lines {
		if !strings.Contains(ln, marker) {
			continue
		}
		for _, body := range lines[i+1:] {
			b := strings.TrimSpace(body)
			if b == "" {
				continue
			}
			if b == "(none heard yet)" {
				return nil
			}
			var out []string
			for _, part := range strings.Split(b, ",") {
				if p := strings.TrimSpace(part); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
	}
	return nil
}

// countClusters counts the cluster-warning lines in the DO-NOT-MERGE section (between
// its heading and the "Deliberate non-merges" heading), excluding the "(none
// applicable yet)" placeholder.
func countClusters(lines []string) int {
	const head = "## DO-NOT-MERGE clusters"
	const tail = "## Deliberate non-merges"
	in := false
	count := 0
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, head):
			in = true
		case strings.HasPrefix(ln, tail):
			return count
		case in:
			b := strings.TrimSpace(ln)
			if b == "" || b == "(none applicable yet)" {
				continue
			}
			count++
		}
	}
	return count
}

// --- comparison + fs helpers ------------------------------------------------

// firstLineDiff returns a short description of the first line where want and got
// diverge (local debugging output only; nothing is committed).
func firstLineDiff(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	for i := range wl {
		var g string
		if i < len(gl) {
			g = gl[i]
		}
		if i >= len(gl) || wl[i] != g {
			return "  line " + strconv.Itoa(i+1) + " historical=" + strconv.Quote(wl[i]) + " generated=" + strconv.Quote(g)
		}
	}
	return "  (generated is longer than historical)"
}

// repoRoot returns the module root (two levels up from this test file at
// internal/spelling/).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func readFileGolden(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // path derives from the read-only extraction dir under test
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func copyFileGolden(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, readFileGolden(t, src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyDirFiles(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), readFileGolden(t, filepath.Join(src, e.Name())), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
