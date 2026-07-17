package repair

import (
	"context"
	"fmt"
	"math"
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

// knownFailedTolSec is how close (seconds) an effective clip window start must be to a
// prior CLIP-REDEGENERATED verdict's recorded clip_start to count as "the same window
// that already failed" - so a re-queued identical tail_clip is skipped rather than
// re-cut and re-transcribed (which would re-degenerate identically). Both values are
// pyRound'd to 1 decimal, so 1s comfortably absorbs rounding without merging distinct
// windows.
const knownFailedTolSec = 1.0

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
	// StartOverrideSec is an OPTIONAL agent-supplied window start (seconds from chapter
	// start). For a TAIL clip (ClipAndSplice) when > 0 it replaces the transcript-derived
	// ClipWindow start, so the agent can relocate a window whose derived cut kept re-
	// degenerating; 0 derives as usual (the historical-port geometry stays byte-identical).
	// The loop is still LOCATED on the transcript for the rotation-adjudication unit
	// regardless. For a MID clip (ClipAndSpliceWindow) it is the interior window START and
	// must be > 0.
	StartOverrideSec float64
	// EndOverrideSec is the interior window END (seconds from chapter start) for a MID clip
	// (ClipAndSpliceWindow), which cuts [StartOverrideSec, EndOverrideSec]. It is unused by
	// the TAIL path (ClipAndSplice), so 0 keeps the tail geometry byte-identical.
	EndOverrideSec float64
	// DecodeTag identifies the decode parameters this attempt uses (the pipeline passes a
	// package const, e.g. "nocontext-v1"). It is recorded on any CLIP-REDEGENERATED verdict
	// this attempt writes, and the known-failed skip fires only when a recorded verdict's
	// tag EQUALS this one - so a legacy verdict written with different (context-conditioned)
	// decode params never blocks a chapter's one fresh attempt under the new params.
	DecodeTag string
}

// ClipResult reports what ClipAndSplice did for one chapter.
type ClipResult struct {
	Chapter     int
	Located     bool    // a tail run was found (LocateTailRun ok AND phrase located in the word stream)
	Verdict     Verdict // the adjudication verdict (empty when no run was located)
	Spliced     bool    // transcripts-repaired/chNNN.txt was written
	ClipHealthy bool    // the fresh clip passed the max-6-gram-x1 health check
	// SkippedKnownFailed is set when the effective window matched a prior
	// CLIP-REDEGENERATED verdict for this chapter, so no cut/transcribe was attempted
	// (Verdict is CLIP-REDEGENERATED, Spliced false). It is distinct from a fresh
	// re-degeneration - no ASR ran - so the stage counts it separately (free retry).
	SkippedKnownFailed bool
	InClip             int
	Unit               string
	Period             int
	ClipStart          float64 // rounded clip start (the splice cut point)
	WordsBefore        int
	WordsAfter         int
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

	// Window start: the transcript-derived snap by default, or the agent's override when
	// supplied. The override relocates a window the derived cut kept re-degenerating; the
	// end geometry (+2s pad) and everything downstream are unchanged, so the derived path
	// stays byte-identical.
	snapped := ClipWindow(req.Transcript, run)
	if req.StartOverrideSec > 0 {
		snapped = req.StartOverrideSec
	}
	clipStart := pyRound(snapped, 1)
	res.ClipStart = clipStart

	// Known-failed skip: if this exact window already re-degenerated in a prior round
	// UNDER THE SAME DECODE PARAMS (a CLIP-REDEGENERATED verdict at the same clip_start
	// whose recorded decode_tag matches this attempt's), cutting and re-transcribing the
	// identical audio with the identical params would just re-degenerate again - often 20+
	// minutes of wasted ASR. Skip it; the caller counts it separately. A legacy verdict
	// written under different params (empty/differing tag) never matches, so the chapter
	// still gets exactly one fresh attempt under the new params.
	if knownFailedWindow(req.WorkDir, req.Chapter, clipStart, req.DecodeTag) {
		res.SkippedKnownFailed = true
		res.Verdict = VerdictClipRedegenerated
		return res, nil
	}

	// The clip filename is keyed on the chapter AND the effective window start, so a
	// same-window resume reuses the file but a RELOCATED window (StartOverrideSec) forces
	// a fresh cut instead of reusing the prior window's audio spliced at the new boundary.
	// The start is encoded as an INTEGER number of deciseconds (clipStart is already
	// pyRound'd to 0.1s, so this is exact and collision-free) - the stem must stay
	// dot-free because the ASR backends derive the raw-output name from the input stem
	// (asr.RawOutputName), and a '.' in the name makes their splitext-style naming
	// disagree with outputStem (mlx wrote t005-660.json for a t005-660.0.flac input),
	// breaking the read-back. A stale old-window clip lingers in clips/ until the scratch
	// purge - acceptable.
	clipFlac := filepath.Join(req.WorkDir, ClipsDir, fmt.Sprintf("t%03d-%d.flac", req.Chapter, int(math.Round(clipStart*10))))
	// Cut [snapped, chend - snapped + 2] (tail_clip_check.py adds 2s of tail so the real
	// ending is fully captured), transcribe prompt-free, and health-check.
	clipText, healthy, err := cutTranscribeHealth(ctx, req, "clip", clipFlac, snapped, req.ChapterEnd-snapped+2)
	if err != nil {
		return res, err
	}
	res.ClipHealthy = healthy

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
		if err := MergeTailVerdict(req.WorkDir, tailVerdict(run, adj, clipStart, req.ChapterEnd, res.Verdict, req.DecodeTag)); err != nil {
			return res, err
		}
		return res, nil
	}

	newText, before, after := Splice(req.Transcript, clipStart, clipText)
	res.WordsBefore, res.WordsAfter = before, after
	line := buildRepairLine(req.Chapter, adj.Verdict, adj.Unit, run.Count, run.LoopWords,
		snapped, req.ChapterEnd, run.LoopSeconds(), run.ClaimedWPS(), before, after)
	if err := writeSplice(req.WorkDir, req.Chapter, newText, line,
		tailVerdict(run, adj, clipStart, req.ChapterEnd, adj.Verdict, req.DecodeTag)); err != nil {
		return res, err
	}
	res.Spliced = true
	return res, nil
}

// ClipAndSpliceWindow runs the mechanical MID-CHAPTER interior repair for one chapter:
// snap the agent's [StartOverrideSec, EndOverrideSec] window to segment edges, cut and
// re-transcribe just that window prompt-free, health-check it, and (unless it re-
// degenerated) splice the fresh window BETWEEN the intact head (before start) and tail
// (after end) - writing transcripts-repaired/chNNN.txt, appending repairs.log, and
// merging a MID-REPAIRED verdict carrying both window bounds.
//
// Unlike ClipAndSplice (which LOCATES a tail loop on the transcript), the interior window
// is supplied by the agent (req.StartOverrideSec / req.EndOverrideSec must bound a valid
// window). This path never runs LocateTailRun and never rotation-adjudicates: the
// ClipHealthy check alone gates the splice. A re-degenerated clip records a mid CLIP-
// REDEGENERATED verdict (with both bounds) and keeps the original; an identical window
// already known to fail is skipped without cutting. ctx cancellation propagates from the
// cut/transcribe calls.
func ClipAndSpliceWindow(ctx context.Context, req ClipSpliceRequest) (ClipResult, error) {
	res := ClipResult{Chapter: req.Chapter}
	if req.StartOverrideSec <= 0 || req.EndOverrideSec <= req.StartOverrideSec {
		return res, fmt.Errorf("mid-clip ch%03d: invalid window [%g, %g]", req.Chapter, req.StartOverrideSec, req.EndOverrideSec)
	}

	// Snap the window to segment edges so the head/tail keep whole segments and the cut
	// covers exactly the dropped middle (see snapWindow); both bounds are recorded (and
	// keyed for the known-failed skip) rounded to 0.1s.
	snappedStart, snappedEnd := snapWindow(req.Transcript, req.StartOverrideSec, req.EndOverrideSec)
	clipStart := pyRound(snappedStart, 1)
	clipEnd := pyRound(snappedEnd, 1)
	res.ClipStart = clipStart

	// Known-failed skip: this exact interior window already re-degenerated under the same
	// params, so re-cutting the identical audio would fail identically. Skip; the caller
	// counts it as a free retry.
	if knownFailedMidWindow(req.WorkDir, req.Chapter, clipStart, clipEnd, req.DecodeTag) {
		res.SkippedKnownFailed = true
		res.Verdict = VerdictClipRedegenerated
		return res, nil
	}

	// The clip filename encodes BOTH bounds in deciseconds (clipStart/clipEnd are pyRound'd
	// to 0.1s, so the ints are exact and collision-free). The stem must stay dot-free (the
	// ASR backends derive the raw-output name from the input stem - a '.' breaks the read-
	// back; see the tail clip's t%03d-%d.flac note). The m-prefix distinguishes a mid clip
	// from a tail clip (t-prefix) for the same chapter, so the two never collide in clips/.
	clipFlac := filepath.Join(req.WorkDir, ClipsDir, fmt.Sprintf("m%03d-%d-%d.flac", req.Chapter, int(math.Round(clipStart*10)), int(math.Round(clipEnd*10))))
	// Cut [snappedStart, snappedEnd], transcribe prompt-free, and health-check.
	clipText, healthy, err := cutTranscribeHealth(ctx, req, "mid-clip", clipFlac, snappedStart, snappedEnd-snappedStart)
	if err != nil {
		return res, err
	}
	res.ClipHealthy = healthy

	// An unhealthy fresh window re-degenerated: record a mid CLIP-REDEGENERATED verdict
	// (both bounds, so a later identical mid_clip is skipped) and keep the original.
	if !res.ClipHealthy {
		res.Verdict = VerdictClipRedegenerated
		if err := MergeTailVerdict(req.WorkDir, midVerdict(req.Chapter, clipStart, clipEnd, VerdictClipRedegenerated, req.DecodeTag)); err != nil {
			return res, err
		}
		return res, nil
	}

	newText, before, after := SpliceWindow(req.Transcript, req.StartOverrideSec, req.EndOverrideSec, clipText)
	res.WordsBefore, res.WordsAfter = before, after
	if err := writeSplice(req.WorkDir, req.Chapter, newText,
		buildMidRepairLine(req.Chapter, snappedStart, snappedEnd, before, after),
		midVerdict(req.Chapter, clipStart, clipEnd, VerdictMidRepaired, req.DecodeTag)); err != nil {
		return res, err
	}
	res.Verdict = VerdictMidRepaired
	res.Spliced = true
	return res, nil
}

// cutTranscribeHealth is the shared mechanical middle of both splice paths: ensure the
// clips dir, cut the [startSec, startSec+durSec] window into clipFlac when absent (a
// present clip is reused for resume), transcribe it prompt-free, normalize, and run the
// max-6-gram-x1 health check. label ("clip" / "mid-clip") only shapes the wrapped error
// text so each public func's messages stay unchanged. It returns the fresh clip's plain
// text and whether it passed the health check.
func cutTranscribeHealth(ctx context.Context, req ClipSpliceRequest, label, clipFlac string, startSec, durSec float64) (string, bool, error) {
	if err := os.MkdirAll(filepath.Join(req.WorkDir, ClipsDir), 0o750); err != nil {
		return "", false, err
	}
	if !fsutil.IsFile(clipFlac) {
		srcFlac := filepath.Join(req.WorkDir, audio.ChaptersDir, audio.ChapterFileName(req.Chapter))
		if err := req.Cut(ctx, srcFlac, clipFlac, startSec, durSec); err != nil {
			return "", false, fmt.Errorf("cut %s ch%03d: %w", label, req.Chapter, err)
		}
	}
	rawJSON, err := req.Transcribe(ctx, clipFlac)
	if err != nil {
		return "", false, fmt.Errorf("transcribe %s ch%03d: %w", label, req.Chapter, err)
	}
	clipT, err := transcript.Normalize(rawJSON, transcript.Meta{Chapter: req.Chapter})
	if err != nil {
		return "", false, fmt.Errorf("normalize %s ch%03d: %w", label, req.Chapter, err)
	}
	clipText := transcript.PlainText(clipT)
	return clipText, ClipHealthy(clipText), nil
}

// writeSplice persists a successful splice's three durable artifacts in the fixed order
// both splice paths use: the repaired chapter text, the repairs.log line, then the merged
// verdict. Each caller builds its own newText/logLine/verdict (Splice vs SpliceWindow,
// tail vs mid line and verdict).
func writeSplice(workDir string, chapter int, newText, logLine string, verdict TailVerdict) error {
	if err := WriteRepaired(workDir, chapter, newText); err != nil {
		return err
	}
	if err := AppendRepairLog(workDir, logLine); err != nil {
		return err
	}
	return MergeTailVerdict(workDir, verdict)
}

// tailVerdict assembles the persisted verdict record for a chapter. decodeTag records the
// decode params this attempt ran under, so a later known-failed skip only fires when the
// params match (see knownFailedWindow).
func tailVerdict(run TailRun, adj Adjudication, clipStart, chapterEnd float64, verdict Verdict, decodeTag string) TailVerdict {
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
		DecodeTag:  decodeTag,
	}
}

// midVerdict assembles the persisted verdict record for a mid_clip interior repair: only
// the window bounds (clipStart/clipEnd), the verdict and the decode tag are meaningful
// (the tail-specific loop/adjudication fields stay zero). ClipEnd > 0 marks it a mid
// verdict, which the residual auto-accept and the mid known-failed skip both key on.
func midVerdict(chapter int, clipStart, clipEnd float64, verdict Verdict, decodeTag string) TailVerdict {
	return TailVerdict{
		Chapter:   chapter,
		ClipStart: clipStart,
		ClipEnd:   clipEnd,
		ClipSecs:  pyRound(clipEnd-clipStart, 1),
		Verdict:   verdict,
		DecodeTag: decodeTag,
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

// redegenVerdictFor returns chapter's recorded verdict when the ledger carries a
// CLIP-REDEGENERATED verdict for it whose recorded decode_tag EQUALS decodeTag - the
// shared precondition of both known-failed skips. It reports ok=false (and a zero verdict)
// for an unreadable/absent ledger, a chapter with no verdict, a non-redegenerated verdict,
// or a tag mismatch, so the first attempt at any window is never skipped and a legacy
// verdict written under different (context-conditioned) params never blocks a fresh
// attempt. Callers add only the window-bound comparison (tail vs mid).
func redegenVerdictFor(workDir string, chapter int, decodeTag string) (TailVerdict, bool) {
	byCh, err := TailVerdictsByChapter(workDir)
	if err != nil {
		return TailVerdict{}, false
	}
	v, ok := byCh[chapter]
	if !ok || v.Verdict != VerdictClipRedegenerated || v.DecodeTag != decodeTag {
		return TailVerdict{}, false
	}
	return v, true
}

// knownFailedWindow reports whether chapter's tail_verdicts.json already carries a
// CLIP-REDEGENERATED TAIL verdict whose recorded clip_start is within knownFailedTolSec of
// clipStart AND whose decode_tag matches - i.e. this exact window was already cut,
// re-transcribed, and re-degenerated in a prior round UNDER THE SAME DECODE PARAMS, so
// re-attempting it is pointless (see redegenVerdictFor for the shared precondition). A mid
// verdict (IsMidWindow) records a bounded interior window and must NOT block a tail
// re-attempt; legacy tail verdicts have ClipEnd == 0, so this is a no-op for all
// pre-mid-clip data.
func knownFailedWindow(workDir string, chapter int, clipStart float64, decodeTag string) bool {
	v, ok := redegenVerdictFor(workDir, chapter, decodeTag)
	return ok && !v.IsMidWindow() && math.Abs(v.ClipStart-clipStart) <= knownFailedTolSec
}

// knownFailedMidWindow is knownFailedWindow's counterpart for a mid_clip: it fires when
// chapter's ledger already carries a CLIP-REDEGENERATED MID verdict (IsMidWindow) whose
// BOTH bounds are within knownFailedTolSec of clipStart/clipEnd AND whose decode_tag
// matches - i.e. this exact interior window was already cut, re-transcribed, and re-
// degenerated under the same params, so re-attempting it is pointless. A tail verdict
// never matches a mid request (see redegenVerdictFor for the shared precondition).
func knownFailedMidWindow(workDir string, chapter int, clipStart, clipEnd float64, decodeTag string) bool {
	v, ok := redegenVerdictFor(workDir, chapter, decodeTag)
	return ok && v.IsMidWindow() &&
		math.Abs(v.ClipStart-clipStart) <= knownFailedTolSec &&
		math.Abs(v.ClipEnd-clipEnd) <= knownFailedTolSec
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
