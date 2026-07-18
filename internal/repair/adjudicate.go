package repair

import "strings"

// Verdict is the tail-clip adjudication outcome (adjudicate_tails.py). It answers the
// one narrow question a repeated tail line poses: is the FIRST occurrence authentic?
type Verdict string

const (
	// VerdictFabricated: the loop's repeating unit is ABSENT from the fresh
	// prompt-free clip - a line hallucinated over silence/music past the real ending.
	VerdictFabricated Verdict = "FABRICATED"
	// VerdictBenign: the unit is present at most twice - a real closing line the ASR
	// echoed over the fade. Kept once, the repeats dropped.
	VerdictBenign Verdict = "BENIGN"
	// VerdictClipRedegenerated: the unit is present more than twice - the fresh clip
	// degenerated identically, so the clip is not trustworthy; keep the original.
	VerdictClipRedegenerated Verdict = "CLIP-REDEGENERATED"
	// VerdictMidRepaired: a bounded MID-CHAPTER interior window was cut, re-transcribed
	// prompt-free, health-checked, and spliced between the intact head and tail. Unlike
	// the tail verdicts it is not an authenticity judgment (there is no closing line to
	// adjudicate) - the ClipHealthy check is the guard, and this records that a healthy
	// interior window replaced an interior degeneration loop.
	VerdictMidRepaired Verdict = "MID-REPAIRED"
	// VerdictTailRepaired: an agent-directed TAIL window was cut, re-transcribed prompt-free,
	// health-checked, and spliced to the chapter end. It is the run-less counterpart of the
	// FABRICATED/BENIGN tail splice - the mechanical locator found NO loop (a short repeat like
	// a 3x phrase is below the 6-gram threshold's reach), so the adjudicator supplied a
	// clip_start_sec and there is no located run to rotation-adjudicate. Like MID-REPAIRED it
	// is not an authenticity judgment: the ClipHealthy check is the sole guard, and this
	// records that a healthy fresh window replaced an unlocatable tail degeneration.
	VerdictTailRepaired Verdict = "TAIL-REPAIRED"
)

// adjRedegenMax is adjudicate_tails.py's boundary: in_clip <= 2 is a real echoed
// line, > 2 means the clip re-degenerated. A real closing line is spoken exactly once.
const adjRedegenMax = 2

// Adjudication is the recovered loop unit and the verdict from testing it against the
// fresh clip.
type Adjudication struct {
	Unit    string  // the loop's repeating unit (its period), or the phrase's first 3 words as fallback
	Period  int     // the recovered period length, 0 when none was found
	InClip  int     // best rotation match count in the fresh clip
	Verdict Verdict // FABRICATED / BENIGN / CLIP-REDEGENERATED
}

// Adjudicate ports adjudicate_tails.py: recover the tail loop's repeating unit (its
// period) from the run tokens, then test every ROTATION of that unit against the
// prompt-free clip transcription (a periodic run's phase is arbitrary, so matching
// the unit as written can score 0 on a line that is genuinely present). The verdict
// is driven purely by how many times the unit appears in the clip.
//
// chapterText is the chapter's plain transcript text (PlainText of the same
// transcript LocateTailRun read); run.LoopWords selects the tail token slice;
// run.Phrase is the fallback when no period is recovered. clipText is the fresh clip
// transcription.
func Adjudicate(run TailRun, chapterText, clipText string) Adjudication {
	toks := strings.Fields(normAdj(chapterText))
	lw := max(run.LoopWords, 0)
	var runToks []string
	if lw > 0 && lw <= len(toks) {
		runToks = toks[len(toks)-lw:]
	} else if lw > 0 {
		runToks = toks // lw larger than the stream (divergent tokenizations): use all, matching Python's toks[len-lw:] clamp
	}

	period := 0
	if len(runToks) >= 4 {
		period = periodOf(runToks)
	}

	var unit string
	if period > 0 {
		unit = strings.Join(runToks[:period], " ")
	} else {
		// Fallback: the phrase's first 3 words, adjudicate-normalized.
		pw := strings.Fields(normAdj(run.Phrase))
		if len(pw) > 3 {
			pw = pw[:3]
		}
		unit = strings.Join(pw, " ")
	}

	clipNorm := normAdj(clipText)
	u := strings.Fields(unit)
	inClip := 0
	if len(u) == 0 {
		// Python: rotations or [unit] -> [unit] (a single ""), clipNorm.count("").
		// Degenerate; treat as absent (no meaningful unit to find).
		inClip = 0
	} else {
		for i := range u {
			rot := strings.Join(rotate(u, i), " ")
			if c := strings.Count(clipNorm, rot); c > inClip {
				inClip = c
			}
		}
		// A rotation can straddle the clip's own start/end boundary; accept a solid
		// contiguous fragment (>= 60% of the line) as present.
		if inClip == 0 && len(u) >= 6 {
			fragLen := max(int(float64(len(u))*0.6), 4)
			for i := range u {
				r := rotate(u, i)
				if fragLen > len(r) {
					continue
				}
				frag := strings.Join(r[:fragLen], " ")
				if strings.Contains(clipNorm, frag) {
					inClip = 1
					break
				}
			}
		}
	}

	var v Verdict
	switch {
	case inClip == 0:
		v = VerdictFabricated
	case inClip <= adjRedegenMax:
		v = VerdictBenign
	default:
		v = VerdictClipRedegenerated
	}
	return Adjudication{Unit: unit, Period: period, InClip: inClip, Verdict: v}
}

// periodOf ports adjudicate_tails.py period_of: the smallest p (1..min(16, n/2)) for
// which the run is at least 90% periodic (toks[i] == toks[i+p] for >= 90% of i). It
// is the recovered length of the loop's repeating unit, or 0 when none qualifies.
func periodOf(toks []string) int {
	n := len(toks)
	hi := min(n/2+1, 16)
	for p := 1; p < hi; p++ {
		matches := 0
		for i := 0; i < n-p; i++ {
			if toks[i] == toks[i+p] {
				matches++
			}
		}
		denom := max(n-p, 1)
		if float64(matches)/float64(denom) >= 0.9 {
			return p
		}
	}
	return 0
}

// rotate returns u rotated left by i (u[i:] + u[:i]).
func rotate(u []string, i int) []string {
	out := make([]string, 0, len(u))
	out = append(out, u[i:]...)
	out = append(out, u[:i]...)
	return out
}
