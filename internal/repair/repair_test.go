package repair

import (
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// --- synthetic transcript builders ------------------------------------------

// tok is one timed token.
type tok struct {
	w          string
	start, end float64
}

// buildTranscript groups tokens into segments of perSeg words each, with each
// segment's Text the space-joined words (leading space, mirroring whisper) and its
// Words carrying the timings. PlainText of the result is the tokens space-joined.
func buildTranscript(chapter, perSeg int, toks []tok) transcript.Transcript {
	var segs []transcript.Segment
	for i := 0; i < len(toks); i += perSeg {
		end := min(i+perSeg, len(toks))
		chunk := toks[i:end]
		var text strings.Builder
		words := make([]transcript.Word, 0, len(chunk))
		for _, tk := range chunk {
			text.WriteString(" ")
			text.WriteString(tk.w)
			words = append(words, transcript.Word{W: tk.w, Start: tk.start, End: tk.end})
		}
		segs = append(segs, transcript.Segment{
			ID:    len(segs),
			Start: chunk[0].start,
			End:   chunk[len(chunk)-1].end,
			Text:  text.String(),
			Words: words,
		})
	}
	return transcript.Transcript{Schema: transcript.Schema, Chapter: chapter, Segments: segs}
}

// loopTokens builds a token stream: n leading narration words (t=0..), then the loop
// phrase repeated reps times, each loop token spanning dt seconds starting at loopT.
func loopTokens(narration int, phrase []string, reps int, loopStart, dt float64) []tok {
	var toks []tok
	t := 0.0
	for i := range narration {
		toks = append(toks, tok{w: "word" + itoa(i), start: t, end: t + 0.3})
		t += 0.3
	}
	t = loopStart
	for range reps {
		for _, w := range phrase {
			toks = append(toks, tok{w: w, start: t, end: t + dt})
			t += dt
		}
	}
	return toks
}

func itoa(i int) string {
	if i == 0 {
		return "a"
	}
	// distinct short lowercase tokens so no accidental 6-gram repeats in narration
	const letters = "abcdefghijklmnopqrstuvwxyz"
	var b strings.Builder
	for i > 0 {
		b.WriteByte(letters[i%26])
		i /= 26
	}
	return b.String()
}

// --- LocateTailRun ----------------------------------------------------------

func TestLocateTailRun_FindsTailCluster(t *testing.T) {
	phrase := []string{"and", "then", "he", "ran", "away", "fast"}
	// 60 narration words, loop repeated 12x starting at t=100, each word 0.05s.
	toks := loopTokens(60, phrase, 12, 100.0, 0.05)
	tr := buildTranscript(3, 8, toks)

	run, ok := LocateTailRun(tr, 200.0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !run.Located {
		t.Fatal("expected the loop to be located in the word stream")
	}
	if run.Phrase != strings.Join(phrase, " ") {
		t.Errorf("phrase = %q, want %q", run.Phrase, strings.Join(phrase, " "))
	}
	if run.Count < 10 {
		t.Errorf("count = %d, want >= 10", run.Count)
	}
	// loop_words = tokens from the run start to the end = 12*6 = 72.
	if run.LoopWords != 72 {
		t.Errorf("loop_words = %d, want 72", run.LoopWords)
	}
	if run.LoopStartT != 100.0 {
		t.Errorf("loop_start_t = %v, want 100.0", run.LoopStartT)
	}
	if run.ImpossibleByDuration() {
		// 72 words over 100s is ~0.72 w/s, well under 7.5.
		t.Error("did not expect impossible-by-duration for a slow loop")
	}
}

func TestLocateTailRun_BelowThresholdOrShort(t *testing.T) {
	// Too few tokens: 10 narration words, no loop.
	short := buildTranscript(1, 5, loopTokens(10, nil, 0, 0, 0))
	if _, ok := LocateTailRun(short, 30); ok {
		t.Error("expected ok=false for < 50 tokens")
	}
	// Enough tokens but no 6-gram repeats (all distinct): 60 distinct words.
	distinct := buildTranscript(1, 8, loopTokens(60, nil, 0, 0, 0))
	if _, ok := LocateTailRun(distinct, 30); ok {
		t.Error("expected ok=false when the top 6-gram is below threshold")
	}
}

func TestLocateTailRun_ImpossibleByDuration(t *testing.T) {
	phrase := []string{"do", "do", "do", "do", "do", "do"}
	// 60 narration + a fast crammed loop: 20 reps * 6 = 120 words in ~1.2s.
	toks := loopTokens(60, phrase, 20, 300.0, 0.01)
	tr := buildTranscript(2, 8, toks)
	run, ok := LocateTailRun(tr, 301.5)
	if !ok || !run.Located {
		t.Fatalf("expected located run, ok=%v located=%v", ok, run.Located)
	}
	if !run.ImpossibleByDuration() {
		t.Errorf("expected impossible-by-duration; claimed_wps=%v", run.ClaimedWPS())
	}
}

// --- Adjudicate -------------------------------------------------------------

func adjRun(loopWords int, phrase string) TailRun {
	return TailRun{Chapter: 1, LoopWords: loopWords, Phrase: phrase, Located: true}
}

func TestAdjudicate_Fabricated(t *testing.T) {
	unit := []string{"and", "then", "he", "ran", "away", "fast"}
	chapterText := strings.Repeat("and then he ran away fast ", 8)
	run := adjRun(len(unit)*8, strings.Join(unit, " "))
	// Clip has the real ending, NOT the loop unit.
	adj := Adjudicate(run, chapterText, "he walked to the door and left the room")
	if adj.Verdict != VerdictFabricated {
		t.Errorf("verdict = %s, want FABRICATED (in_clip=%d unit=%q)", adj.Verdict, adj.InClip, adj.Unit)
	}
	if adj.Period != 6 {
		t.Errorf("period = %d, want 6", adj.Period)
	}
	if adj.Unit != "and then he ran away fast" {
		t.Errorf("unit = %q", adj.Unit)
	}
}

func TestAdjudicate_Benign(t *testing.T) {
	unit := []string{"the", "sun", "set", "over", "the", "hills"}
	chapterText := strings.Repeat("the sun set over the hills ", 6)
	run := adjRun(len(unit)*6, strings.Join(unit, " "))
	// The clip carries the real closing line exactly once.
	adj := Adjudicate(run, chapterText, "she smiled and the sun set over the hills at last")
	if adj.Verdict != VerdictBenign {
		t.Errorf("verdict = %s, want BENIGN (in_clip=%d)", adj.Verdict, adj.InClip)
	}
	if adj.InClip != 1 {
		t.Errorf("in_clip = %d, want 1", adj.InClip)
	}
}

func TestAdjudicate_ClipRedegenerated(t *testing.T) {
	unit := []string{"over", "and", "over", "again", "it", "went"}
	chapterText := strings.Repeat("over and over again it went ", 6)
	run := adjRun(len(unit)*6, strings.Join(unit, " "))
	clip := strings.Repeat("over and over again it went ", 4) // the clip looped too
	adj := Adjudicate(run, chapterText, clip)
	if adj.Verdict != VerdictClipRedegenerated {
		t.Errorf("verdict = %s, want CLIP-REDEGENERATED (in_clip=%d)", adj.Verdict, adj.InClip)
	}
	if adj.InClip <= 2 {
		t.Errorf("in_clip = %d, want > 2", adj.InClip)
	}
}

func TestAdjudicate_RotationMatch(t *testing.T) {
	// The run begins mid-phrase, so the recovered unit is a ROTATION of the real
	// sentence; matching must still find it in the clip.
	unit := []string{"ran", "away", "fast", "and", "then", "he"} // rotation of the real line
	chapterText := strings.Repeat("ran away fast and then he ", 6)
	run := adjRun(len(unit)*6, strings.Join(unit, " "))
	// Clip speaks the line in its natural phase.
	adj := Adjudicate(run, chapterText, "and then he ran away fast into the night")
	if adj.Verdict != VerdictBenign {
		t.Errorf("verdict = %s, want BENIGN via rotation (in_clip=%d unit=%q)", adj.Verdict, adj.InClip, adj.Unit)
	}
}

func TestAdjudicate_PhraseFallbackWhenNoPeriod(t *testing.T) {
	// A short non-periodic run: fewer than 4 tokens -> no period -> phrase fallback.
	run := adjRun(3, "closing line here now stop end") // Phrase is the 6-gram
	chapterText := "some narration closing line here"
	adj := Adjudicate(run, chapterText, "unrelated clip text")
	if adj.Period != 0 {
		t.Errorf("period = %d, want 0 (fallback)", adj.Period)
	}
	// Fallback unit = first 3 words of the phrase.
	if adj.Unit != "closing line here" {
		t.Errorf("unit = %q, want %q", adj.Unit, "closing line here")
	}
}

// --- Splice -----------------------------------------------------------------

func TestSplice_KeepsSegmentsBelowClipStart(t *testing.T) {
	tr := transcript.Transcript{
		Schema: transcript.Schema, Chapter: 1,
		Segments: []transcript.Segment{
			{ID: 0, Start: 0, End: 10, Text: " Hello world."},
			{ID: 1, Start: 10, End: 20, Text: " Foo bar."},        // End == clip_start, kept (<=)
			{ID: 2, Start: 20, End: 30, Text: " loop loop loop."}, // End > clip_start, dropped
		},
	}
	text, before, after := Splice(tr, 20.0, "fresh ending here")

	if strings.Contains(text, "loop") {
		t.Errorf("spliced text kept a dropped segment: %q", text)
	}
	if text != "Hello world. Foo bar. fresh ending here" {
		t.Errorf("text = %q", text)
	}
	// before = words of PlainText = "Hello world. Foo bar. loop loop loop." = 7.
	if before != 7 {
		t.Errorf("wordsBefore = %d, want 7", before)
	}
	if after != 7 {
		t.Errorf("wordsAfter = %d, want 7", after)
	}
}

// --- ClipHealthy ------------------------------------------------------------

func TestClipHealthy(t *testing.T) {
	if !ClipHealthy("the quick brown fox jumps over the lazy dog by night") {
		t.Error("expected a clean clip to be healthy")
	}
	if ClipHealthy(strings.Repeat("a b c d e f ", 4)) {
		t.Error("expected a looping clip (6-gram repeated) to be unhealthy")
	}
}

// --- buildRepairLine (byte-identical to build_repairs.py) -------------------

func TestBuildRepairLine_MatchesHistoricalFormat(t *testing.T) {
	// Reproduce the HW05 repairs.log ch002 line exactly.
	loopSecs := 1380.658 - 524.6    // 856.058
	claimedWPS := 1069.0 / loopSecs // 1.2487...
	line := buildRepairLine(2, VerdictFabricated, "wait hump", 532, 1069,
		515.7, 1380.658, loopSecs, claimedWPS, 2290, 3257)
	want := "- ch002 [FABRICATED]: spliced clip at 515.7s (+865.0s). loop 'wait hump' x532 claimed 1069w in 856.06s (1.2 w/s). words 2290 -> 3257 (+967)"
	if line != want {
		t.Errorf("line mismatch:\n got %q\nwant %q", line, want)
	}
}

func TestBuildRepairLine_NegativeDelta(t *testing.T) {
	// A BENIGN splice that removes words prints a "-NN" delta.
	line := buildRepairLine(5, VerdictBenign, "what in the hells is going on", 9, 63,
		1049.2, 1049.2+15.3, 7.26, 8.7, 2435, 2376)
	if !strings.Contains(line, "words 2435 -> 2376 (-59)") {
		t.Errorf("missing negative delta: %q", line)
	}
	if !strings.HasPrefix(line, "- ch005 [BENIGN]: spliced clip at 1049.2s (+15.3s).") {
		t.Errorf("unexpected prefix: %q", line)
	}
}

// --- pyRound / pyFloatStr ---------------------------------------------------

func TestPyRoundAndFloatStr(t *testing.T) {
	cases := []struct {
		v    float64
		nd   int
		want string
	}{
		{865.0, 1, "865.0"},
		{515.7, 1, "515.7"},
		{856.058, 2, "856.06"},
		{7.2, 2, "7.2"},
		{1.2487, 1, "1.2"},
		{19.5, 1, "19.5"},
		{2.675, 2, "2.67"}, // half-to-even via float representation, like Python
	}
	for _, c := range cases {
		got := pyFloatStr(pyRound(c.v, c.nd))
		if got != c.want {
			t.Errorf("pyFloatStr(pyRound(%v,%d)) = %q, want %q", c.v, c.nd, got, c.want)
		}
	}
}

// --- AdoptFresh -------------------------------------------------------------

func TestAdoptFresh(t *testing.T) {
	const dur = 600.0                                   // 10 minutes
	orig := AdoptStats{Words: 1500, DurationSec: dur}   // 150 wpm, plausible
	origBad := AdoptStats{Words: 300, DurationSec: dur} // 30 wpm, implausible-low
	cases := []struct {
		name      string
		original  AdoptStats
		fresh     AdoptStats
		wantAdopt bool
	}{
		{"fresh collapsed keep original", orig, AdoptStats{Words: 300, DurationSec: dur}, false},
		{"fresh crammed keep original", orig, AdoptStats{Words: 4000, DurationSec: dur}, false},
		{"fresh fixes bad original", origBad, AdoptStats{Words: 1500, DurationSec: dur}, true},
		{"both plausible fresh longer adopt", orig, AdoptStats{Words: 2000, DurationSec: dur}, true},
		{"both plausible fresh shorter keep", orig, AdoptStats{Words: 1000, DurationSec: dur}, false},
		{"fresh zero duration keep", orig, AdoptStats{Words: 1500, DurationSec: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := AdoptFresh(c.original, c.fresh)
			if d.Adopt != c.wantAdopt {
				t.Errorf("Adopt = %v (%s), want %v", d.Adopt, d.Reason, c.wantAdopt)
			}
			if d.Reason == "" {
				t.Error("expected a non-empty reason")
			}
		})
	}
}
