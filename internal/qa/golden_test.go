// Golden tests that replay the REAL historical extraction work dirs against the Go
// QA engine and compare its output to the Python originals' on-disk output.
//
// # Env gate + read-only guarantee
//
// These tests run only when AUDIOSILO_EXTRACTION_DIR points at a local checkout of
// the historical extraction work dirs AND the referenced book dirs exist; otherwise
// they t.Skip cleanly (CI and any machine without the private data skip). The
// extraction dir is treated as strictly READ-ONLY: every engine run happens in a
// t.TempDir() copy, and the source raw transcripts are only ever read.
//
// # Why the in-repo expectations are numbers-only
//
// The hard contract of this task: no transcript text (and no character/place name)
// may enter the repo. So this file hard-codes only NUMBERS and structural facts -
// chapter numbers, the historical retranscribe queue, the tail-rate spot-check
// chapters. Every text comparison (the qa_report.md byte-prefix) happens at runtime
// against the locally-parsed historical file; nothing about the books' prose is
// committed here.
package qa

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

const extractionEnv = "AUDIOSILO_EXTRACTION_DIR"

// goldenBook describes one historical book replay. Everything here is a number or a
// path fragment - never book text.
type goldenBook struct {
	name string // short label
	rel  string // path under the extraction dir
	// tailChapters are chapters the tail-rate detector must flag (verified locally
	// against tail_rate_scan.py before hard-coding). Empty = no spot check.
	tailChapters []int
}

func goldenBooks() []goldenBook {
	return []goldenBook{
		// HW05: the canonical 84-chapter book; qa_report.md queue [2, 29, 67].
		{name: "HW05", rel: "hedge-wizard/work5", tailChapters: []int{5, 6, 8}},
		// RLF03: the second book, whose qa_report.md carries a non-empty
		// retranscribe queue [52, 56, 90].
		{name: "RLF03", rel: "living-forge/work3"},
	}
}

// extractionDir returns the extraction root, or skips the test when the env var is
// unset (the CI / no-data path).
func extractionDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(extractionEnv)
	if dir == "" {
		t.Skipf("%s not set - skipping golden replay (needs the local historical extraction data)", extractionEnv)
	}
	return dir
}

// requireBookDir returns the book's work dir, skipping when it (or its qa_report.md)
// is absent so a partial checkout skips rather than fails.
func requireBookDir(t *testing.T, root string, b goldenBook) string {
	t.Helper()
	work := filepath.Join(root, filepath.FromSlash(b.rel))
	if _, err := os.Stat(filepath.Join(work, ReportMDName)); err != nil {
		t.Skipf("%s missing or has no %s (%v) - skipping", b.rel, ReportMDName, err)
	}
	return work
}

func TestGoldenQAReport(t *testing.T) {
	root := extractionDir(t)
	for _, b := range goldenBooks() {
		t.Run(b.name, func(t *testing.T) {
			src := requireBookDir(t, root, b)
			tmp := t.TempDir()
			durations := buildGoldenWorkDir(t, src, tmp)

			rep, err := Run(Input{WorkDir: tmp, Durations: durations})
			if err != nil {
				t.Fatalf("qa.Run: %v", err)
			}
			if err := WriteReport(tmp, rep); err != nil {
				t.Fatalf("qa.WriteReport: %v", err)
			}

			// Core assertion: the historical qa_report.md is a byte-for-byte PREFIX
			// of the generated one (our md renders the same five qa_sweep.py sections
			// first, then appends the extra detector sections the Python printed to
			// stdout).
			historical := readFileGolden(t, filepath.Join(src, ReportMDName))
			generated := readFileGolden(t, filepath.Join(tmp, ReportMDName))
			assertBytePrefix(t, historical, generated)

			// Self-check the parse: the report's retranscribe queue equals the queue
			// parsed from the historical md's "## retranscribe queue" line.
			wantQueue := parseHistoricalQueue(t, historical)
			if !slices.Equal(rep.RetranscribeQueue, wantQueue) {
				t.Errorf("retranscribe queue: report %v, historical md %v", rep.RetranscribeQueue, wantQueue)
			}

			// Numeric spot check: the named chapters must appear among the tail-rate
			// findings (verified locally against tail_rate_scan.py).
			for _, ch := range b.tailChapters {
				if !hasTailChapter(rep.TailRate, ch) {
					t.Errorf("tail-rate findings missing chapter %d; got %v", ch, tailChapters(rep.TailRate))
				}
			}
		})
	}
}

// buildGoldenWorkDir populates tmp with everything qa.Run reads: manifest.json (for
// durations), transcripts-json/ (normalized from the source's immutable raw layer via
// internal/transcript, mirroring the daemon's sanitizing stage), and transcripts-
// repaired/ when the source has it (the multi-loop detector consults it). It copies
// no audio and never writes into the source. It returns the parsed durations.
func buildGoldenWorkDir(t *testing.T, src, tmp string) map[int]float64 {
	t.Helper()

	// Durations from the source manifest.
	var man struct {
		Chapters []struct {
			Chapter  int     `json:"chapter"`
			Duration float64 `json:"duration"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(readFileGolden(t, filepath.Join(src, "manifest.json")), &man); err != nil {
		t.Fatalf("parse manifest.json: %v", err)
	}
	durations := make(map[int]float64, len(man.Chapters))
	for _, c := range man.Chapters {
		durations[c.Chapter] = c.Duration
	}

	// Normalize each raw transcript into the temp transcripts-json/. Reading the raw
	// layer from the read-only source and writing the normalized copy into tmp
	// exercises the real adapter on real data without copying the raw layer.
	rawDir := filepath.Join(src, transcript.RawDir)
	jsonDir := filepath.Join(tmp, transcript.JSONDir)
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		t.Fatalf("read %s: %v", transcript.RawDir, err)
	}
	normalized := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		n, ok := transcript.ParseChapter(e.Name())
		if !ok {
			continue
		}
		raw := readFileGolden(t, filepath.Join(rawDir, e.Name()))
		tr, err := transcript.Normalize(raw, transcript.Meta{Chapter: n})
		if err != nil {
			t.Fatalf("normalize ch%03d: %v", n, err)
		}
		if err := transcript.WriteNormalized(jsonDir, tr); err != nil {
			t.Fatalf("write normalized ch%03d: %v", n, err)
		}
		normalized++
	}
	if normalized == 0 {
		t.Fatalf("no raw transcripts normalized from %s", rawDir)
	}

	// Copy transcripts-repaired/ (plain-text repaired chapters) when present.
	if repSrc := filepath.Join(src, transcript.RepairedDir); dirExists(repSrc) {
		copyPlainFiles(t, repSrc, filepath.Join(tmp, transcript.RepairedDir))
	}
	return durations
}

// --- assertions + parsing helpers (numbers/structure only) ------------------

// assertBytePrefix fails when historical is not a byte-for-byte prefix of generated,
// reporting the first differing line (test output is local only, so a small text
// excerpt from the LOCAL historical file at the divergence is acceptable for
// debugging - nothing is committed).
func assertBytePrefix(t *testing.T, historical, generated []byte) {
	t.Helper()
	if bytes.HasPrefix(generated, historical) {
		return
	}
	hl := strings.Split(string(historical), "\n")
	gl := strings.Split(string(generated), "\n")
	for i := range hl {
		var g string
		if i < len(gl) {
			g = gl[i]
		}
		if i >= len(gl) || hl[i] != g {
			t.Errorf("qa_report.md diverges from historical at line %d:\n  historical: %q\n  generated:  %q",
				i+1, hl[i], g)
			// A little following context.
			for j := i + 1; j < i+4 && j < len(hl); j++ {
				var gg string
				if j < len(gl) {
					gg = gl[j]
				}
				t.Logf("  line %d historical=%q generated=%q", j+1, hl[j], gg)
			}
			return
		}
	}
	// historical has no differing line but is longer than generated (not a prefix).
	t.Errorf("generated qa_report.md (%d bytes) is shorter than historical (%d bytes)", len(generated), len(historical))
}

// parseHistoricalQueue reads the "## retranscribe queue" section of a historical
// qa_report.md and returns the chapter list ("[2, 29, 67]" -> {2,29,67}, "empty" ->
// nil). It fails the test if the section is missing.
func parseHistoricalQueue(t *testing.T, md []byte) []int {
	t.Helper()
	lines := strings.Split(string(md), "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) != "## retranscribe queue" {
			continue
		}
		for _, body := range lines[i+1:] {
			body = strings.TrimSpace(body)
			if body == "" {
				continue
			}
			body = strings.TrimPrefix(body, "- ")
			if body == "empty" {
				return nil
			}
			return parseIntList(t, body)
		}
	}
	t.Fatalf("historical qa_report.md has no '## retranscribe queue' section")
	return nil
}

// parseIntList parses a Python list literal like "[2, 29, 67]" into ints.
func parseIntList(t *testing.T, s string) []int {
	t.Helper()
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			t.Fatalf("parse queue element %q: %v", part, err)
		}
		out = append(out, n)
	}
	return out
}

func hasTailChapter(hits []TailRateHit, ch int) bool {
	for _, h := range hits {
		if h.Chapter == ch {
			return true
		}
	}
	return false
}

func tailChapters(hits []TailRateHit) []int {
	out := make([]int, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Chapter)
	}
	return out
}

// --- small fs helpers -------------------------------------------------------

func readFileGolden(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // path derives from the read-only extraction dir under test
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// copyPlainFiles copies every regular file from src into dst (created if absent). It
// is flat (the transcript layers hold no subdirectories).
func copyPlainFiles(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b := readFileGolden(t, filepath.Join(src, e.Name()))
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
