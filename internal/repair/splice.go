package repair

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// Work-dir artifact names the repair flow writes (mirroring the historical layout).
const (
	// RepairsLogName is the append-only human log of every splice.
	RepairsLogName = "repairs.log"
	// TailVerdictsName is the machine-readable per-chapter verdict ledger, merged
	// across rounds and re-read by the qa_adjudicating stage on re-entry.
	TailVerdictsName = "tail_verdicts.json"
	// ClipsDir holds the cut audio windows + their fresh transcriptions (scratch).
	ClipsDir = "clips"

	repairsLogHeader = "# repairs - clip-spliced tails (raw is immutable)"
)

// Splice ports build_repairs.py's splice: keep every segment whose end <= clipStart
// (the fresh clip is ground truth from clipStart onward), concatenate their ORIGINAL
// text as the head, append the fresh clip text, and collapse surrounding whitespace.
// It returns the repaired chapter text plus the before/after word counts (Python's
// len(text.split())). clipStart must be the ROUNDED clip start (build_repairs uses
// h["clip_start"], the 1-decimal value, for the kept-segment filter).
func Splice(t transcript.Transcript, clipStart float64, clipText string) (text string, wordsBefore, wordsAfter int) {
	var head strings.Builder
	for _, s := range t.Segments {
		if s.End <= clipStart {
			head.WriteString(s.Text)
		}
	}
	h := strings.TrimSpace(head.String())
	clip := strings.TrimSpace(clipText)
	newText := strings.TrimSpace(h + " " + clip)
	wordsBefore = len(strings.Fields(transcript.PlainText(t)))
	wordsAfter = len(strings.Fields(newText))
	return newText, wordsBefore, wordsAfter
}

// WriteRepaired writes the spliced chapter text to transcripts-repaired/chNNN.txt
// (via transcript.WriteText, which appends the trailing newline and refuses the
// immutable raw layer). apply_corrections prefers this layer over transcripts-text
// per chapter.
func WriteRepaired(workDir string, chapter int, text string) error {
	return transcript.WriteText(filepath.Join(workDir, transcript.RepairedDir), chapter, text)
}

// buildRepairLine formats one repairs.log entry byte-identically to build_repairs.py.
// snappedStart is the UNROUNDED window start (tail_clip_check's `start`); the log's
// "at Xs" prints round(start,1) and "(+Ys)" prints round(chend-start,1), matching the
// Python which stored clip_start/clip_secs from the same unrounded start. loopSecs
// and claimedWPS come from the located run (negative when the run was unlocated, which
// the Python printed as "None").
func buildRepairLine(chapter int, verdict Verdict, unit string, count, loopWords int, snappedStart, chapterEnd, loopSecs, claimedWPS float64, before, after int) string {
	clipStart := pyRound(snappedStart, 1)
	clipSecs := pyRound(chapterEnd-snappedStart, 1)
	loopSecsStr := "None"
	if loopSecs >= 0 {
		loopSecsStr = pyFloatStr(pyRound(loopSecs, 2))
	}
	wpsStr := "None"
	if claimedWPS >= 0 {
		wpsStr = pyFloatStr(pyRound(claimedWPS, 1))
	}
	return fmt.Sprintf(
		"- ch%03d [%s]: spliced clip at %ss (+%ss). loop %s x%d claimed %dw in %ss (%s w/s). words %d -> %d (%+d)",
		chapter, verdict,
		pyFloatStr(clipStart), pyFloatStr(clipSecs),
		qa.PyRepr(qa.TruncateRunes(unit, repairUnitTrunc)), count,
		loopWords, loopSecsStr, wpsStr,
		before, after, after-before,
	)
}

// AppendRepairLog appends one already-formatted line to workDir/repairs.log, creating
// the file with a header on first write. Unlike the batch build_repairs.py (which
// rewrote the whole file with a header and a footer summary), the M5 pipeline splices
// chapters incrementally, so this appends; the per-entry line format is preserved
// exactly (the summary footer is omitted - it was a batch artifact).
func AppendRepairLog(workDir, line string) error {
	path := filepath.Join(workDir, RepairsLogName)
	var buf strings.Builder
	if !fsutil.IsFile(path) {
		buf.WriteString(repairsLogHeader)
		buf.WriteString("\n\n")
	} else {
		existing, err := os.ReadFile(path) //nolint:gosec // path derives from the book's own work dir
		if err != nil {
			return err
		}
		buf.Write(existing)
	}
	buf.WriteString(line)
	buf.WriteString("\n")
	return fsutil.WriteFileAtomic(path, []byte(buf.String()), 0o644)
}

// TailVerdict is one chapter's persisted adjudication record (a compact subset of the
// historical tail_verdicts.json: the fields downstream stages and re-entry rounds
// need - the bulky clip_text is intentionally omitted).
type TailVerdict struct {
	Chapter    int     `json:"ch"`
	Count      int     `json:"count"`
	Phrase     string  `json:"phrase"`
	LoopStartT float64 `json:"loop_start_t"`
	LoopWords  int     `json:"loop_words"`
	ClipStart  float64 `json:"clip_start"`
	ClipSecs   float64 `json:"clip_secs"`
	Unit       string  `json:"unit"`
	Period     int     `json:"period"`
	InClip     int     `json:"in_clip"`
	Verdict    Verdict `json:"verdict"`
}

// LoadTailVerdicts reads workDir/tail_verdicts.json, returning an empty slice when the
// file is absent (a first round).
func LoadTailVerdicts(workDir string) ([]TailVerdict, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, TailVerdictsName)) //nolint:gosec // path derives from the book's own work dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []TailVerdict
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", TailVerdictsName, err)
	}
	return out, nil
}

// MergeTailVerdict upserts v into workDir/tail_verdicts.json by chapter (a re-entry
// round replaces a chapter's prior verdict), writing the result sorted by chapter as
// pretty JSON with a trailing newline.
func MergeTailVerdict(workDir string, v TailVerdict) error {
	existing, err := LoadTailVerdicts(workDir)
	if err != nil {
		return err
	}
	replaced := false
	for i := range existing {
		if existing[i].Chapter == v.Chapter {
			existing[i] = v
			replaced = true
			break
		}
	}
	if !replaced {
		existing = append(existing, v)
	}
	sort.Slice(existing, func(i, j int) bool { return existing[i].Chapter < existing[j].Chapter })
	out, err := json.MarshalIndent(existing, "", " ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, TailVerdictsName), append(out, '\n'), 0o644)
}
