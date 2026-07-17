// Package repair is the mechanical port of the historical tail-clip machinery that
// turns a degeneration-flagged ASR chapter back into trustworthy text without any
// agent: it locates a repeated tail loop, cuts and re-transcribes the affected audio
// window prompt-free, decides whether the loop was a real closing line or a
// hallucination over silence, and splices the fresh window over the garble.
//
// It is a faithful Go port of three hand-run Python scripts (hedge-wizard/work5,
// living-forge/work3): tail_clip_check.py (locate the run + duration plausibility +
// clip cut), adjudicate_tails.py (recover the loop's repeating unit and test every
// rotation against the fresh clip) and build_repairs.py (splice the kept head onto
// the fresh clip and log it). The thresholds and quirks are CONTRACT - the same
// numbers those 11+ books validated - and are preserved verbatim, each documented
// with the Python file it came from. Where a piece has no Python source (the full
// -chapter adoption call the human made by eye), it is an adapted heuristic that
// encodes EXTRACTION-AUDIO.md's invariant ("never blindly adopt the fresh run") and
// is marked as adapted rather than ported.
//
// The package depends on internal/transcript (the normalized transcript shape it
// reads) and internal/fsutil, but NOT on internal/asr or ffmpeg: audio cutting and
// clip transcription are injected as function values (ClipCutter, TranscribeClip)
// so the whole flow stays unit-testable with fakes.
package repair

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// RetranscribeDir is the work-dir subdir holding fresh full-chapter re-transcriptions
// (scratch). repair owns the re-transcription scratch naming; scratch reclaims it.
const RetranscribeDir = "retranscribe"

// Detector thresholds and clip geometry - ported verbatim from tail_clip_check.py.
// Do not tune these without re-validating against the historical books; they are the
// contract, not a default.
const (
	// TailGramThreshold is tail_clip_check.py THRESHOLD (3, not HW03's 5): the top
	// 6-gram must repeat at least this many times for a chapter to carry a locatable
	// tail run. A 6-gram repeated 3x is already not natural prose.
	TailGramThreshold = 3

	tailMinToks = 50  // tail_clip_check.py: skip a chapter with < 50 tokens
	clipPad     = 8.0 // tail_clip_check.py PAD: seconds of real narration kept before the loop
	clipMin     = 12.0
	wpsNorm     = 2.5 // tail_clip_check.py WPS: ~150 words/min of narration
	impossibleX = 3.0 // tail_clip_check.py: > 3x normal rate is impossible-by-duration

	repairUnitTrunc = 44 // build_repairs.py: unit[:44] before repr in the log line
)

// TailRun is the located repeated tail loop of a chapter, the output of
// tail_clip_check.py analyse(): the most-common 6-gram, its repeat count, and the
// maximal cluster of its occurrences that reaches the END of the word stream (an
// earlier version located by the head word and clipped nearly the whole chapter -
// the cluster-to-end fix is why this shape exists).
type TailRun struct {
	Chapter    int
	Count      int     // repeat count of the top 6-gram (>= TailGramThreshold)
	Phrase     string  // the top 6-gram, apostrophe-preserving normalized, space-joined
	LoopStartT float64 // start timestamp of the run's first word, or -1 when unlocated
	LoopWords  int     // words from the run start to the end of the word stream, 0 when unlocated
	Located    bool    // whether the phrase was found in the word-timestamp stream
	ChapterEnd float64 // the chapter's duration (chend)
}

// LoopSeconds is the run's claimed duration: chend - loop_start (floored at 0.01),
// or -1 when the run was not located (tail_clip_check.py secs). It is the denominator
// of the duration-plausibility check.
func (r TailRun) LoopSeconds() float64 {
	if !r.Located {
		return -1
	}
	return math.Max(r.ChapterEnd-r.LoopStartT, 0.01)
}

// ClaimedWPS is loop_words / loop_seconds, or -1 when unlocated (tail_clip_check.py
// claimed_wps): how many words the run claims per second of audio.
func (r TailRun) ClaimedWPS() float64 {
	s := r.LoopSeconds()
	if s < 0 {
		return -1
	}
	return float64(r.LoopWords) / s
}

// ImpossibleByDuration reports whether the run claims words faster than 3x normal
// narration (tail_clip_check.py impossible_by_duration): a run claiming e.g. 846
// words in 2.4s is fabricated with certainty, no listening required. False when the
// run was not located.
//
// KEPT as ported contract: it is the duration discriminator from tail_clip_check.py,
// exercised by the repair tests / report parity - do not remove it as "unused".
func (r TailRun) ImpossibleByDuration() bool {
	w := r.ClaimedWPS()
	return w >= 0 && w > wpsNorm*impossibleX
}

// LocateTailRun ports tail_clip_check.py analyse(): over a chapter's normalized
// transcript it finds the most-common 6-gram and, when it repeats at least
// TailGramThreshold times, locates the repeated tail run as the maximal cluster of
// that gram's occurrences reaching the END of the word-timestamp stream. ok is false
// (analyse() returned None) when the chapter has fewer than 50 tokens or the top
// 6-gram is below threshold - there is no tail to adjudicate.
//
// chapterEnd is the chapter's duration in seconds (manifest chend). The transcript is
// the normalized audiosilo-transcript/v1 form; its top-level text equals the raw
// openai "text" the Python read, and Word.W equals the raw per-word "word", so the
// port is semantically identical (verified byte-for-byte against HW05).
func LocateTailRun(t transcript.Transcript, chapterEnd float64) (TailRun, bool) {
	toks := strings.Fields(normTail(transcript.PlainText(t)))
	if len(toks) < tailMinToks {
		return TailRun{}, false
	}
	gram, count := qa.TopGram(toks)
	if count < TailGramThreshold {
		return TailRun{}, false
	}
	// Re-normalize the winning gram exactly as the Python does (idempotent for
	// already-normalized tokens, but faithful).
	phrase := strings.Fields(normTail(strings.Join(gram, " ")))

	// Build the word-timestamp stream (words_of): each word normalized+trimmed, empties
	// dropped, carrying its start/end.
	type tw struct {
		w          string
		start, end float64
	}
	var ws []tw
	for _, seg := range t.Segments {
		for _, w := range seg.Words {
			tt := strings.TrimSpace(normTail(w.W))
			if tt != "" {
				ws = append(ws, tw{w: tt, start: w.Start, end: w.End})
			}
		}
	}
	seq := make([]string, len(ws))
	for i := range ws {
		seq[i] = ws[i].w
	}

	// Every start index where the phrase occurs in the word stream.
	var idxs []int
	if len(phrase) > 0 {
		for i := 0; i+len(phrase) <= len(seq); i++ {
			if slices.Equal(seq[i:i+len(phrase)], phrase) {
				idxs = append(idxs, i)
			}
		}
	}

	run := TailRun{
		Chapter:    t.Chapter,
		Count:      count,
		Phrase:     strings.Join(phrase, " "),
		LoopStartT: -1,
		ChapterEnd: chapterEnd,
	}
	if len(idxs) > 0 {
		// The tail run = the maximal cluster of occurrences reaching the end.
		cluster := []int{idxs[len(idxs)-1]}
		for k := len(idxs) - 2; k >= 0; k-- {
			i := idxs[k]
			if cluster[0]-i <= len(phrase)*2 {
				cluster = append([]int{i}, cluster...)
			} else {
				break
			}
		}
		first := cluster[0]
		run.LoopStartT = ws[first].start
		run.LoopWords = len(seq) - first
		run.Located = true
	}
	return run, true
}

// ClipWindow returns the snapped clip start (unrounded) for a located run,
// reproducing tail_clip_check.py's window snapping: want = max(0, loop_start - PAD)
// clamped to chend - MIN_CLIP, then the start snaps DOWN to the end of the last
// segment that finishes at or before want, so the kept head and the fresh clip abut
// exactly (no gap, no overlap). For an unlocated run it degrades to chend - MIN_CLIP.
// It reads only segment end times.
func ClipWindow(t transcript.Transcript, run TailRun) float64 {
	var want float64
	if run.Located {
		want = run.LoopStartT - clipPad
	} else {
		want = run.ChapterEnd - clipMin
	}
	if want < 0 {
		want = 0
	}
	if w := run.ChapterEnd - clipMin; want > w {
		want = w
	}
	start := 0.0
	for _, s := range t.Segments {
		if s.End <= want && s.End > start {
			start = s.End
		}
	}
	return start
}

// --- normalization + n-gram helpers (ported, apostrophe-preserving) ---

// normTail ports tail_clip_check.py norm(): lowercase, and replace every rune not in
// [a-z0-9' ] with a space. Unlike the qa detectors' normalizers, this one KEEPS the
// apostrophe (so "it's" stays one token). Whitespace is not collapsed here; the
// caller's strings.Fields is the equivalent of Python's str.split().
func normTail(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '\'', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// normAdj ports adjudicate_tails.py norm(): lowercase, replace every rune not in
// [a-z0-9 ] with a space (dropping the apostrophe), COLLAPSE whitespace and trim. The
// collapse is load-bearing: without it "unilia, the" -> "unilia  the" and no
// single-spaced probe can match it.
func normAdj(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// --- Python-compatible number + string formatting for the log line ---

// pyRound rounds v to ndigits decimals matching Python's round(): correctly-rounded
// round-half-to-even on the double's TRUE value. Go's math.Round is half-away-from
// -zero and a float multiply (v*10^n) introduces its own rounding error, either of
// which diverges from Python on exact halves (round(2.675, 2) == 2.67, because the
// double nearest 2.675 is 2.67499...). strconv.FormatFloat does the same
// correctly-rounded half-to-even conversion Python's round uses, so routing through
// it reproduces the historical files.
func pyRound(v float64, ndigits int) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', ndigits, 64), 64)
	return r
}

// pyFloatStr formats v the way Python's str(float) does: the shortest decimal that
// round-trips, but ALWAYS with a decimal point (str(865.0) == "865.0", not "865").
// The report/log values are small enough that strconv never falls back to exponent
// form.
func pyFloatStr(v float64) string {
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}
