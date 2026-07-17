package repair

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// clipHealthMax6gram is build_repairs.py's pre-adoption guard: a fresh clip must not
// repeat any 6-gram more than once. A clip that loops has re-degenerated and is never
// adopted blind.
const clipHealthMax6gram = 1

// ClipCutter cuts [startSec, startSec+durSec] of the source FLAC into dstFlac. It is
// injected so internal/repair depends on neither ffmpeg-for-cutting nor a hard exec;
// FFmpegClipCutter is the production implementation and tests use a fake.
type ClipCutter func(ctx context.Context, srcFlac, dstFlac string, startSec, durSec float64) error

// TranscribeClip transcribes the audio clip at clipPath PROMPT-FREE and returns the
// raw backend transcript JSON (any format transcript.Normalize accepts). It is
// injected so this package never imports internal/asr: the pipeline wraps its ASR
// backend, tests return canned JSON. The prompt-free requirement is load-bearing - an
// initial prompt makes the model echo it over sparse audio (an HW03 clip came back
// "Alex Maher." x60).
type TranscribeClip func(ctx context.Context, clipPath string) ([]byte, error)

// ClipSpliceRequest is the input to ClipAndSplice for one flagged chapter.
type ClipSpliceRequest struct {
	WorkDir    string                // the book's work dir
	Chapter    int                   // logical chapter number
	Transcript transcript.Transcript // the chapter's normalized transcript
	ChapterEnd float64               // the chapter's duration (manifest chend)
	Cut        ClipCutter            // audio cutter (FFmpegClipCutter in production)
	Transcribe TranscribeClip        // prompt-free clip transcription
}

// ClipResult reports what ClipAndSplice did for one chapter.
type ClipResult struct {
	Chapter     int
	Located     bool    // a tail run was found (LocateTailRun ok AND phrase located in the word stream)
	Verdict     Verdict // the adjudication verdict (empty when no run was located)
	Spliced     bool    // transcripts-repaired/chNNN.txt was written
	ClipHealthy bool    // the fresh clip passed the max-6-gram-x1 health check
	InClip      int
	Unit        string
	Period      int
	ClipStart   float64 // rounded clip start (the splice cut point)
	WordsBefore int
	WordsAfter  int
}

// ClipAndSplice runs the full mechanical tail repair for one chapter: locate the tail
// run, cut the audio window, transcribe it prompt-free, health-check the clip,
// adjudicate the loop against the fresh clip, and (unless the clip re-degenerated)
// splice the fresh window over the garble - writing transcripts-repaired/chNNN.txt,
// appending repairs.log and merging tail_verdicts.json.
//
// A chapter with no locatable tail run (LocateTailRun returned ok=false, or the phrase
// never appeared in the word-timestamp stream) is a no-op: ClipResult.Located is
// false, no splice, no artifacts. A CLIP-REDEGENERATED verdict (or an unhealthy clip)
// keeps the original and records only the verdict. ctx cancellation propagates from
// the cut/transcribe calls.
func ClipAndSplice(ctx context.Context, req ClipSpliceRequest) (ClipResult, error) {
	res := ClipResult{Chapter: req.Chapter}

	run, ok := LocateTailRun(req.Transcript, req.ChapterEnd)
	if !ok || !run.Located {
		// No 6-gram tail to adjudicate, or the phrase was not in the timed word
		// stream: nothing to clip. The caller records the chapter as unrepaired.
		return res, nil
	}
	res.Located = true

	snapped := ClipWindow(req.Transcript, run)
	clipStart := pyRound(snapped, 1)
	res.ClipStart = clipStart

	// Cut the window: [snapped, chend - snapped + 2] (tail_clip_check.py adds 2s of
	// tail so the real ending is fully captured). Resumable: a present clip is reused.
	clipsDir := filepath.Join(req.WorkDir, ClipsDir)
	if err := os.MkdirAll(clipsDir, 0o750); err != nil {
		return res, err
	}
	clipFlac := filepath.Join(clipsDir, fmt.Sprintf("t%03d.flac", req.Chapter))
	if !fsutil.IsFile(clipFlac) {
		srcFlac := filepath.Join(req.WorkDir, audio.ChaptersDir, audio.ChapterFileName(req.Chapter))
		dur := req.ChapterEnd - snapped + 2
		if err := req.Cut(ctx, srcFlac, clipFlac, snapped, dur); err != nil {
			return res, fmt.Errorf("cut clip ch%03d: %w", req.Chapter, err)
		}
	}

	rawJSON, err := req.Transcribe(ctx, clipFlac)
	if err != nil {
		return res, fmt.Errorf("transcribe clip ch%03d: %w", req.Chapter, err)
	}
	clipT, err := transcript.Normalize(rawJSON, transcript.Meta{Chapter: req.Chapter})
	if err != nil {
		return res, fmt.Errorf("normalize clip ch%03d: %w", req.Chapter, err)
	}
	clipText := transcript.PlainText(clipT)

	res.ClipHealthy = ClipHealthy(clipText)

	adj := Adjudicate(run, transcript.PlainText(req.Transcript), clipText)
	res.Verdict = adj.Verdict
	res.InClip = adj.InClip
	res.Unit = adj.Unit
	res.Period = adj.Period

	// A clip that re-degenerated (verdict says so, or the health check failed) is not
	// trustworthy: keep the original, record the verdict, do not splice. An unhealthy
	// clip is forced to CLIP-REDEGENERATED regardless of the rotation match.
	if adj.Verdict == VerdictClipRedegenerated || !res.ClipHealthy {
		res.Verdict = VerdictClipRedegenerated
		if err := MergeTailVerdict(req.WorkDir, tailVerdict(run, adj, clipStart, req.ChapterEnd, res.Verdict)); err != nil {
			return res, err
		}
		return res, nil
	}

	newText, before, after := Splice(req.Transcript, clipStart, clipText)
	res.WordsBefore, res.WordsAfter = before, after
	if err := WriteRepaired(req.WorkDir, req.Chapter, newText); err != nil {
		return res, err
	}
	line := buildRepairLine(req.Chapter, adj.Verdict, adj.Unit, run.Count, run.LoopWords,
		snapped, req.ChapterEnd, run.LoopSeconds(), run.ClaimedWPS(), before, after)
	if err := AppendRepairLog(req.WorkDir, line); err != nil {
		return res, err
	}
	if err := MergeTailVerdict(req.WorkDir, tailVerdict(run, adj, clipStart, req.ChapterEnd, adj.Verdict)); err != nil {
		return res, err
	}
	res.Spliced = true
	return res, nil
}

// tailVerdict assembles the persisted verdict record for a chapter.
func tailVerdict(run TailRun, adj Adjudication, clipStart, chapterEnd float64, verdict Verdict) TailVerdict {
	return TailVerdict{
		Chapter:    run.Chapter,
		Count:      run.Count,
		Phrase:     run.Phrase,
		LoopStartT: run.LoopStartT,
		LoopWords:  run.LoopWords,
		ClipStart:  clipStart,
		ClipSecs:   pyRound(chapterEnd-clipStart, 1),
		Unit:       adj.Unit,
		Period:     adj.Period,
		InClip:     adj.InClip,
		Verdict:    verdict,
	}
}

// ClipHealthy reports whether a fresh clip transcription did NOT re-degenerate: its
// most-common 6-gram repeats at most once (build_repairs.py's max-6-gram-x1 guard). It
// tokenizes with the same apostrophe-preserving normalizer LocateTailRun uses.
func ClipHealthy(clipText string) bool {
	toks := strings.Fields(normTail(clipText))
	_, count := qa.TopGram(toks)
	return count <= clipHealthMax6gram
}

// FFmpegClipCutter returns a ClipCutter that cuts the window to a mono/16 kHz FLAC.
// It delegates to audio.CutClip so clip audio shares the exact encode path (and so
// stays bit-comparable to the chapter FLACs internal/audio.Split produces); input
// seeking (-ss before -i) matches tail_clip_check.py.
func FFmpegClipCutter(ffmpegPath string) ClipCutter {
	return func(ctx context.Context, srcFlac, dstFlac string, startSec, durSec float64) error {
		return audio.CutClip(ctx, ffmpegPath, srcFlac, dstFlac, startSec, durSec)
	}
}
