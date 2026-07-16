package qa

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// wphOutliers ports qa_sweep.py's words-per-hour outlier check. It considers every
// chapter except chapter 0, computes each chapter's wph (words / hours, 0 when the
// duration is unknown), and flags any chapter more than 2.5 SAMPLE standard
// deviations from the book mean. It returns the chapter count (len(rows)), the mean
// and sample stdev, and the flagged chapters in chapter order.
func wphOutliers(docs []chapterDoc) (chapters int, mean, sd float64, outliers []WPHOutlier) {
	type row struct {
		number  int
		words   int
		minutes float64
		wph     float64
	}
	var rows []row
	var wphs []float64
	for _, d := range docs {
		if d.number == 0 { // chapter 0 is the recap, outside the position model + sweep
			continue
		}
		words := len(strings.Fields(d.fulltext))
		hours := d.duration / 3600
		wph := 0.0
		if hours != 0 {
			wph = float64(words) / hours
		}
		rows = append(rows, row{number: d.number, words: words, minutes: hours * 60, wph: wph})
		wphs = append(wphs, wph)
	}

	outliers = make([]WPHOutlier, 0)
	if len(rows) == 0 {
		return 0, 0, 0, outliers
	}

	var sum float64
	for _, v := range wphs {
		sum += v
	}
	mean = sum / float64(len(wphs))
	sd = sampleStdDev(wphs, mean)

	for _, r := range rows {
		z := 0.0
		if sd != 0 {
			z = (r.wph - mean) / sd
		}
		if math.Abs(z) > wphZThreshold {
			outliers = append(outliers, WPHOutlier{
				Chapter: r.number, Words: r.words, Minutes: r.minutes, WPH: r.wph, Z: z,
			})
		}
	}
	return len(rows), mean, sd, outliers
}

// repeatedRuns ports qa_sweep.py's repeated-segment run detector: runs of >= 3
// consecutive segments whose NORMALIZED text is identical and non-empty. Each run is
// classified end-fade (starts at >= 85% of the chapter) or MID-CHAPTER LOOP.
// Excludes chapter 0. The run that extends to the final segment is flushed after the
// loop, exactly as the Python does.
func repeatedRuns(docs []chapterDoc) []RepeatedRun {
	out := make([]RepeatedRun, 0)
	for _, d := range docs {
		if d.number == 0 {
			continue
		}
		segs := d.t.Segments
		runStart := -1
		runLen := 0
		prev := "" // Python starts prev=None; a non-empty cur can never equal "", so this matches
		emit := func() {
			if runStart == -1 || runLen < repeatRunMin {
				return
			}
			startT := segs[runStart].Start // Python `segs[run_start].get("start", 0) or 0`; Start is always set here
			pos := 0.0
			if d.duration != 0 {
				pos = startT / d.duration
			}
			kind := KindMidChapter
			if pos >= endFadePosition {
				kind = KindEndFade
			}
			out = append(out, RepeatedRun{
				Chapter:  d.number,
				Kind:     kind,
				Length:   runLen,
				StartSec: startT,
				Snippet:  truncateRunes(segs[runStart].Text, snippetLen),
			})
		}
		for i, seg := range segs {
			cur := normalizeUnicode(seg.Text)
			if cur != "" && cur == prev {
				if runStart == -1 {
					runStart = i - 1
					runLen = 2
				} else {
					runLen++
				}
			} else {
				emit()
				runStart = -1
				runLen = 0
			}
			prev = cur
		}
		emit()
	}
	return out
}

// lowConfidence ports qa_sweep.py's low-confidence tally: per chapter (excluding
// chapter 0) the count of words scored below 0.5 and the total word count, with
// book totals and the 5 worst chapters by rate (low / max(total, 1)). The worst
// sort is stable and descending, preserving chapter order for ties (Python's
// reverse=True stable sort).
func lowConfidence(docs []chapterDoc) LowConfidence {
	var rows []LowConfChapter
	var totalLow, totalWords int
	for _, d := range docs {
		if d.number == 0 {
			continue
		}
		low, total := 0, 0
		for _, seg := range d.t.Segments {
			for _, w := range seg.Words {
				total++
				if w.P != nil && *w.P < lowConfProb {
					low++
				}
			}
		}
		rows = append(rows, LowConfChapter{Chapter: d.number, Low: low, Total: total})
		totalLow += low
		totalWords += total
	}

	worst := append([]LowConfChapter(nil), rows...)
	sort.SliceStable(worst, func(i, j int) bool {
		return lowConfRate(worst[i]) > lowConfRate(worst[j])
	})
	if len(worst) > lowConfWorstN {
		worst = worst[:lowConfWorstN]
	}
	if worst == nil {
		worst = make([]LowConfChapter, 0)
	}
	return LowConfidence{TotalLow: totalLow, TotalWords: totalWords, Worst: worst}
}

func lowConfRate(c LowConfChapter) float64 {
	return float64(c.Low) / float64(max(c.Total, 1))
}

// crossSegmentLoops ports cross_segment_scan.py: over each chapter's whole-text
// tokens (INCLUDING chapter 0), the single most common 6-gram (first-seen wins
// ties); flagged when it repeats >= 5 times. The phrase is located by scanning the
// RAW segment texts for a substring match. Results are returned sorted by descending
// count (the order the report prints).
func crossSegmentLoops(docs []chapterDoc) []CrossSegmentHit {
	out := make([]CrossSegmentHit, 0)
	for _, d := range docs {
		toks := strings.Fields(d.fulltext)
		if len(toks) < crossMinTokens {
			continue
		}
		gram, count := topGram(toks)
		if count < crossThreshold {
			continue
		}
		phrase := strings.Join(gram, " ")

		var firstSec, lastSec float64
		found := false
		for _, s := range d.t.Segments {
			if strings.Contains(s.Text, phrase) {
				if !found {
					found = true
					firstSec = s.Start
				}
				lastSec = s.End
			}
		}
		chend := d.duration
		if chend == 0 { // cross_segment_scan.py: `dur.get(n, 0) or 1`
			chend = 1
		}
		hit := CrossSegmentHit{Chapter: d.number, Count: count, Pos: -1, Phrase: phrase}
		if found { // Python `if first_t is not None`: a first_t of exactly 0 IS valid here
			hit.Pos = firstSec / chend * 100
			fs, ls := firstSec, lastSec
			hit.FirstSec, hit.LastSec = &fs, &ls
		}
		out = append(out, hit)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// withinSegmentLoops ports within_segment_scan.py: within each segment of each
// chapter (EXCLUDING chapter 0), the most common 6-gram (first-seen ties) over
// segments with >= 12 tokens; flagged when it repeats >= 8 times. Pos is stored as a
// percentage of the chapter (Python stores the fraction and multiplies at print
// time - pre-multiplying yields the same printed value). Results are in encounter
// order (chapter then segment), matching the Python's unsorted output.
func withinSegmentLoops(docs []chapterDoc) []WithinSegmentHit {
	out := make([]WithinSegmentHit, 0)
	for _, d := range docs {
		if d.number == 0 {
			continue
		}
		for _, seg := range d.t.Segments {
			toks := strings.Fields(seg.Text)
			if len(toks) < withinMinTokens {
				continue
			}
			gram, count := topGram(toks)
			if count < withinThreshold {
				continue
			}
			pos := 0.0
			if d.duration != 0 {
				pos = seg.Start / d.duration * 100
			}
			out = append(out, WithinSegmentHit{
				Chapter: d.number, Count: count, Pos: pos, Phrase: strings.Join(gram, " "),
			})
		}
	}
	return out
}

// multiLoops ports multi_loop_scan.py: over each chapter (INCLUDING chapter 0),
// EVERY 6-gram at or above 5 repeats, iterated in descending-count order (first-seen
// ties) and deduped by the SET of the gram's words, so one loop is not reported as a
// dozen overlapping shingles. Text comes from the transcripts-repaired/ layer when
// present, else the normalized full text, tokenized with the ASCII normalizer (which
// genuinely differs from the Unicode normalizer the repeated-run detector uses).
// Results are sorted (chapter asc, count desc), the order the report prints.
func multiLoops(docs []chapterDoc, workDir string) []MultiLoopFinding {
	out := make([]MultiLoopFinding, 0)
	for _, d := range docs {
		text, src := multiText(d, workDir)
		toks := strings.Fields(normalizeASCII(text))
		if len(toks) < multiMinTokens {
			continue
		}
		grams := loopGrams(toks, multiThreshold)
		if len(grams) == 0 {
			continue
		}
		// Normalize the chapter's segments once: locateMulti searches them per
		// finding, and a degenerate chapter (the interesting case) often carries
		// several findings.
		normSegs := make([]string, len(d.t.Segments))
		for i, s := range d.t.Segments {
			normSegs[i] = normalizeASCII(s.Text)
		}
		for _, lg := range grams {
			phrase := strings.Join(lg.gram, " ")
			out = append(out, locateMulti(d, normSegs, phrase, lg.count, src))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Chapter != out[j].Chapter {
			return out[i].Chapter < out[j].Chapter
		}
		return out[i].Count > out[j].Count
	})
	return out
}

// multiText returns the text and source label ("repaired"/"raw") for a chapter's
// multi-loop scan, preferring transcripts-repaired/chNNN.txt when it exists
// (multi_loop_scan.py reads the repaired layer in preference to the raw text,
// because a repair can uncover a second loop the first was masking).
func multiText(d chapterDoc, workDir string) (text, src string) {
	repaired := filepath.Join(workDir, transcript.RepairedDir, transcript.TextName(d.number))
	if raw, err := os.ReadFile(repaired); err == nil { //nolint:gosec // path derives from the book's own work dir
		return string(raw), "repaired"
	}
	return d.fulltext, "raw"
}

// locateMulti finds where phrase first sits in the chapter's segments (normSegs is
// the pre-normalized segment text, index-aligned with d.t.Segments) and builds the
// finding. It replicates multi_loop_scan.py's `phrase[:24] in norm(seg.text)` match
// (ASCII-normalizing the segment text) and its Python truthiness EXACTLY: `at = seg.start`
// then `pos = at/dur*100 if at else -1` - a segment starting at exactly 0 is FALSY in
// Python, so it yields pos -1 and a "?" location, not a real 0% position.
func locateMulti(d chapterDoc, normSegs []string, phrase string, count int, src string) MultiLoopFinding {
	needle := truncateRunes(phrase, 24)
	var at float64
	located := false
	for i, ns := range normSegs {
		if strings.Contains(ns, needle) {
			at = d.t.Segments[i].Start
			located = true
			break
		}
	}
	f := MultiLoopFinding{Chapter: d.number, Count: count, Pos: -1, Phrase: phrase, Source: src}
	if located && at != 0 { // Python `if at`: 0.0 is falsy, so an at of exactly 0 is treated as not located
		f.AtSec = &at
		// Guard the division: an explicit 0 duration would make Pos +Inf, which
		// json.Marshal rejects (failing the whole report). The Python original
		// simply crashed here (ZeroDivisionError), so Pos -1 ("position unknown",
		// AtSec still carrying the located time) is our graceful choice.
		if d.duration != 0 {
			f.Pos = at / d.duration * 100
		}
	}
	f.MidChapter = f.Pos >= 0 && f.Pos < multiMidMax
	return f
}

// tailRateOutliers ports tail_rate_scan.py: over each chapter (INCLUDING chapter 0),
// the final 12 timed words; a chapter is flagged when those 12 words span little
// enough time that they exceed 4.5 words/second - a rate real narration cannot
// sustain. A chapter with fewer than 17 timed words is skipped. In the raw
// openai-whisper JSON a word could carry a null timestamp, which the Python drops;
// normalization has already resolved those to concrete floats, so here every word
// carries timestamps and only the non-empty-word filter remains. Results are sorted
// by descending wps (the order the report prints).
func tailRateOutliers(docs []chapterDoc) []TailRateHit {
	out := make([]TailRateHit, 0)
	for _, d := range docs {
		type timed struct {
			word       string
			start, end float64
		}
		var ws []timed
		for _, seg := range d.t.Segments {
			for _, w := range seg.Words {
				trimmed := strings.TrimSpace(w.W)
				if trimmed == "" {
					continue
				}
				ws = append(ws, timed{word: trimmed, start: w.Start, end: w.End})
			}
		}
		if len(ws) < tailWords+tailSkipSlack {
			continue
		}
		tail := ws[len(ws)-tailWords:]
		span := math.Max(tail[len(tail)-1].end-tail[0].start, tailSpanFloor)
		wps := float64(tailWords) / span
		if wps > tailMaxWPS {
			words := make([]string, len(tail))
			for i, w := range tail {
				words[i] = w.word
			}
			out = append(out, TailRateHit{
				Chapter: d.number,
				WPS:     round1(wps),
				Span:    round2(span),
				Tail:    truncateRunes(strings.Join(words, " "), tailTextLen),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].WPS > out[j].WPS })
	return out
}

// --- shared tokenization + n-gram helpers ---

// normalizeUnicode ports qa_sweep.py's normalize: lowercase every letter/digit,
// keep whitespace, replace every other character with a space, then collapse runs of
// whitespace. Python's str.isalnum()/isspace() are Unicode-aware, so this uses
// unicode.IsLetter||IsDigit and unicode.IsSpace over runes.
func normalizeUnicode(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsSpace(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// normalizeASCII ports multi_loop_scan.py's norm: lowercase, replace every character
// outside [a-z0-9 ] with a space, then collapse whitespace. This is deliberately the
// ASCII-regex behavior of that script and NOT the Unicode-aware normalizeUnicode -
// the two detectors genuinely differ (e.g. an accented letter survives one and is
// dropped by the other), so both are ported faithfully.
func normalizeASCII(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// topGram returns the most common 6-gram in toks and its count, breaking ties by
// first appearance (Python Counter.most_common(1) over an insertion-ordered
// Counter). Returns (nil, 0) when there are fewer than 6 tokens.
func topGram(toks []string) ([]string, int) {
	counts, order, keys := gramCounts(toks)
	best := -1
	var bestGram []string
	for i, g := range order {
		if c := counts[keys[i]]; c > best { // strict > keeps the first-seen gram on ties
			best = c
			bestGram = g
		}
	}
	if bestGram == nil {
		return nil, 0
	}
	return bestGram, best
}

// loopGram is one 6-gram at or above the multi-loop threshold.
type loopGram struct {
	gram  []string
	count int
}

// loopGrams returns every distinct 6-gram in toks with count >= threshold, in
// descending-count order (first-seen ties), deduped by the SET of its words
// (multi_loop_scan.py's frozenset(gram) key). It sorts an index slice so the
// comparator reuses the keys gramCounts already built instead of re-joining each
// gram on every comparison (almost every window of prose is distinct, so that
// would be O(n log n) string joins over ~len(toks) grams).
func loopGrams(toks []string, threshold int) []loopGram {
	counts, order, keys := gramCounts(toks)
	idx := make([]int, len(order))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { // stable: equal counts keep first-seen order
		return counts[keys[idx[a]]] > counts[keys[idx[b]]]
	})
	var out []loopGram
	seen := make(map[string]struct{})
	for _, i := range idx {
		c := counts[keys[i]]
		if c < threshold {
			break
		}
		g := order[i]
		wk := wordSetKey(g)
		if _, ok := seen[wk]; ok {
			continue
		}
		seen[wk] = struct{}{}
		out = append(out, loopGram{gram: g, count: c})
	}
	return out
}

// gramCounts counts every consecutive 6-gram in toks, returning the counts, the
// distinct grams in first-appearance order, and each distinct gram's map key
// (computed once here so callers never rebuild it).
func gramCounts(toks []string) (map[string]int, [][]string, []string) {
	counts := make(map[string]int)
	var order [][]string
	var keys []string
	for i := 0; i+gramSize <= len(toks); i++ {
		g := toks[i : i+gramSize]
		key := gramKey(g)
		if _, ok := counts[key]; !ok {
			order = append(order, append([]string(nil), g...))
			keys = append(keys, key)
		}
		counts[key]++
	}
	return counts, order, keys
}

// gramKey joins a gram's tokens with a NUL separator (which sanitized transcript text
// never contains), so distinct token sequences never collide.
func gramKey(g []string) string { return strings.Join(g, "\x00") }

// wordSetKey is the frozenset(gram) dedup key: the gram's distinct words, sorted and
// NUL-joined, so overlapping shingles of one loop share a key.
func wordSetKey(g []string) string {
	set := make(map[string]struct{}, len(g))
	for _, w := range g {
		set[w] = struct{}{}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x00")
}

// truncateRunes returns the first n runes of s (Python's s[:n] over a str, which is
// rune-indexed), or s when it is already short enough.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// round1/round2 round to 1/2 decimals for the tail-rate report. Go's math.Round is
// half-away-from-zero where Python's round is half-to-even; the stored values can
// differ only in the rare exact-half case and are never golden-compared (the tail
// section has no historical file), so the micro-deviation is deliberate.
func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
