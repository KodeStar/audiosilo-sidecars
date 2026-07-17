// Golden tests replaying the REAL historical tail-clip artifacts (hedge-wizard/work5)
// against the Go port, to prove the mechanical pieces are byte/threshold-identical to
// tail_clip_check.py / adjudicate_tails.py / build_repairs.py.
//
// # Env gate + read-only guarantee
//
// These run only when AUDIOSILO_EXTRACTION_DIR points at a checkout of the historical
// extraction work dirs AND the referenced artifacts exist; otherwise they t.Skip (CI
// and any machine without the private data skip). The extraction dir is strictly
// READ-ONLY: transcripts are normalized into memory and every comparison is against a
// value parsed from the historical files at runtime, so NO book text (or character
// name) is committed to this repo - the expectations live in the historical JSON, not
// here.
//
// # What is and is not golden-reproducible
//
// The three pieces reproducible from the on-disk artifacts alone are validated here:
// run LOCATION (tail_clip_report.json count/phrase/loop_start_t/loop_words/clip_start
// /clip_secs), ROTATION ADJUDICATION (tail_verdicts.json unit/period/in_clip/verdict,
// re-derived from the stored clip_text), and SPLICE word counts (repairs.log
// before->after). The one piece that is NOT reproducible - the ffmpeg clip cut and the
// fresh whisper transcription of it - needs the original FLACs and the ASR model; it is
// covered by synthetic + fake-transcribe tests (clip_test.go) instead, and this file
// consumes the historical clip_text rather than regenerating it.
package repair

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

const extractionEnv = "AUDIOSILO_EXTRACTION_DIR"

// hw05 is the one historical book carrying tail-clip artifacts.
const hw05Rel = "hedge-wizard/work5"

func extractionDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(extractionEnv)
	if dir == "" {
		t.Skipf("%s not set - skipping golden replay (needs the local historical extraction data)", extractionEnv)
	}
	return dir
}

// clipReportEntry mirrors the fields of tail_clip_report.json this test compares.
type clipReportEntry struct {
	Chapter    int      `json:"ch"`
	Count      int      `json:"count"`
	Phrase     string   `json:"phrase"`
	LoopStartT *float64 `json:"loop_start_t"`
	LoopWords  *int     `json:"loop_words"`
	ClipStart  float64  `json:"clip_start"`
	ClipSecs   float64  `json:"clip_secs"`
	ClipText   string   `json:"clip_text"`
}

type verdictEntry struct {
	Chapter int     `json:"ch"`
	Unit    string  `json:"unit"`
	Period  *int    `json:"period"`
	InClip  int     `json:"in_clip"`
	Verdict Verdict `json:"verdict"`
}

// requireArtifact returns the HW05 work dir, skipping when the tail artifacts are absent.
func requireHW05(t *testing.T, root string) string {
	t.Helper()
	work := filepath.Join(root, filepath.FromSlash(hw05Rel))
	for _, f := range []string{"tail_clip_report.json", "tail_verdicts.json", "repairs.log", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(work, f)); err != nil {
			t.Skipf("%s missing %s (%v) - skipping", hw05Rel, f, err)
		}
	}
	return work
}

// loadHW05 reads the manifest durations and normalizes every raw transcript into an
// in-memory map (chapter -> transcript), never writing to the source.
func loadHW05(t *testing.T, work string) (map[int]transcript.Transcript, map[int]float64) {
	t.Helper()
	var man struct {
		Chapters []struct {
			Chapter  int     `json:"chapter"`
			Duration float64 `json:"duration"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(readGolden(t, filepath.Join(work, "manifest.json")), &man); err != nil {
		t.Fatalf("parse manifest.json: %v", err)
	}
	dur := make(map[int]float64, len(man.Chapters))
	for _, c := range man.Chapters {
		dur[c.Chapter] = c.Duration
	}

	rawDir := filepath.Join(work, transcript.RawDir)
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		t.Fatalf("read %s: %v", transcript.RawDir, err)
	}
	trs := make(map[int]transcript.Transcript)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		n, ok := transcript.ParseChapter(e.Name())
		if !ok {
			continue
		}
		tr, err := transcript.Normalize(readGolden(t, filepath.Join(rawDir, e.Name())), transcript.Meta{Chapter: n})
		if err != nil {
			t.Fatalf("normalize ch%03d: %v", n, err)
		}
		trs[n] = tr
	}
	if len(trs) == 0 {
		t.Fatalf("no raw transcripts normalized from %s", rawDir)
	}
	return trs, dur
}

func TestGoldenLocateTailRun(t *testing.T) {
	work := requireHW05(t, extractionDir(t))
	trs, dur := loadHW05(t, work)

	var report []clipReportEntry
	if err := json.Unmarshal(readGolden(t, filepath.Join(work, "tail_clip_report.json")), &report); err != nil {
		t.Fatalf("parse tail_clip_report.json: %v", err)
	}
	if len(report) == 0 {
		t.Fatal("empty tail_clip_report.json")
	}

	for _, g := range report {
		tr, ok := trs[g.Chapter]
		if !ok {
			t.Errorf("ch%03d in report but no transcript", g.Chapter)
			continue
		}
		run, ok := LocateTailRun(tr, dur[g.Chapter])
		if !ok {
			t.Errorf("ch%03d: LocateTailRun returned ok=false, but the report has an entry", g.Chapter)
			continue
		}
		if run.Count != g.Count {
			t.Errorf("ch%03d count = %d, want %d", g.Chapter, run.Count, g.Count)
		}
		if run.Phrase != g.Phrase {
			t.Errorf("ch%03d phrase = %q, want %q", g.Chapter, run.Phrase, g.Phrase)
		}
		if g.LoopStartT == nil {
			if run.Located {
				t.Errorf("ch%03d: located but historical loop_start_t is null", g.Chapter)
			}
		} else if !run.Located {
			t.Errorf("ch%03d: not located but historical has loop_start_t %v", g.Chapter, *g.LoopStartT)
		} else if !approx(run.LoopStartT, *g.LoopStartT, 1e-9) {
			t.Errorf("ch%03d loop_start_t = %v, want %v", g.Chapter, run.LoopStartT, *g.LoopStartT)
		}
		if g.LoopWords != nil && run.LoopWords != *g.LoopWords {
			t.Errorf("ch%03d loop_words = %d, want %d", g.Chapter, run.LoopWords, *g.LoopWords)
		}
		// Window snapping: clip_start (rounded) and clip_secs (from the unrounded start).
		snapped := ClipWindow(tr, run)
		if !approx(pyRound(snapped, 1), g.ClipStart, 1e-9) {
			t.Errorf("ch%03d clip_start = %v, want %v", g.Chapter, pyRound(snapped, 1), g.ClipStart)
		}
		if !approx(pyRound(dur[g.Chapter]-snapped, 1), g.ClipSecs, 1e-9) {
			t.Errorf("ch%03d clip_secs = %v, want %v", g.Chapter, pyRound(dur[g.Chapter]-snapped, 1), g.ClipSecs)
		}
	}
}

func TestGoldenAdjudicate(t *testing.T) {
	work := requireHW05(t, extractionDir(t))
	trs, dur := loadHW05(t, work)

	// Index the clip-check report by chapter for its clip_text + located loop.
	var report []clipReportEntry
	if err := json.Unmarshal(readGolden(t, filepath.Join(work, "tail_clip_report.json")), &report); err != nil {
		t.Fatalf("parse tail_clip_report.json: %v", err)
	}
	byCh := make(map[int]clipReportEntry, len(report))
	for _, r := range report {
		byCh[r.Chapter] = r
	}

	var verdicts []verdictEntry
	if err := json.Unmarshal(readGolden(t, filepath.Join(work, "tail_verdicts.json")), &verdicts); err != nil {
		t.Fatalf("parse tail_verdicts.json: %v", err)
	}
	if len(verdicts) == 0 {
		t.Fatal("empty tail_verdicts.json")
	}

	for _, g := range verdicts {
		rep, ok := byCh[g.Chapter]
		if !ok {
			t.Errorf("ch%03d has a verdict but no clip-check entry", g.Chapter)
			continue
		}
		tr := trs[g.Chapter]
		run, ok := LocateTailRun(tr, dur[g.Chapter])
		if !ok {
			t.Errorf("ch%03d: LocateTailRun ok=false", g.Chapter)
			continue
		}
		adj := Adjudicate(run, transcript.PlainText(tr), rep.ClipText)
		if adj.Verdict != g.Verdict {
			t.Errorf("ch%03d verdict = %s, want %s (in_clip=%d)", g.Chapter, adj.Verdict, g.Verdict, adj.InClip)
		}
		if adj.Unit != g.Unit {
			t.Errorf("ch%03d unit = %q, want %q", g.Chapter, adj.Unit, g.Unit)
		}
		wantPeriod := 0
		if g.Period != nil {
			wantPeriod = *g.Period
		}
		if adj.Period != wantPeriod {
			t.Errorf("ch%03d period = %d, want %d", g.Chapter, adj.Period, wantPeriod)
		}
		if adj.InClip != g.InClip {
			t.Errorf("ch%03d in_clip = %d, want %d", g.Chapter, adj.InClip, g.InClip)
		}
	}
}

var repairLineRe = regexp.MustCompile(`^- ch(\d+) .*words (\d+) -> (\d+)`)

func TestGoldenSplice(t *testing.T) {
	work := requireHW05(t, extractionDir(t))
	trs, _ := loadHW05(t, work)

	var report []clipReportEntry
	if err := json.Unmarshal(readGolden(t, filepath.Join(work, "tail_clip_report.json")), &report); err != nil {
		t.Fatalf("parse tail_clip_report.json: %v", err)
	}
	byCh := make(map[int]clipReportEntry, len(report))
	for _, r := range report {
		byCh[r.Chapter] = r
	}

	logBytes := readGolden(t, filepath.Join(work, "repairs.log"))
	checked := 0
	for _, line := range splitLines(string(logBytes)) {
		m := repairLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ch, _ := strconv.Atoi(m[1])
		wantBefore, _ := strconv.Atoi(m[2])
		wantAfter, _ := strconv.Atoi(m[3])
		rep, ok := byCh[ch]
		if !ok {
			t.Errorf("ch%03d in repairs.log but no clip-check entry", ch)
			continue
		}
		_, before, after := Splice(trs[ch], rep.ClipStart, rep.ClipText)
		if before != wantBefore || after != wantAfter {
			t.Errorf("ch%03d splice words = %d -> %d, want %d -> %d", ch, before, after, wantBefore, wantAfter)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no repairs.log entries parsed")
	}
	t.Logf("splice-checked %d chapters", checked)
}

// --- helpers ----------------------------------------------------------------

func readGolden(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // path derives from the read-only extraction dir under test
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func approx(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
