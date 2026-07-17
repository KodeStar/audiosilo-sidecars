package repair

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// rawOpenAI builds a minimal openai-whisper raw JSON with the given plain text as its
// single segment, which transcript.Normalize accepts.
func rawOpenAI(text string) []byte {
	doc := map[string]any{
		"text":     text,
		"language": "en",
		"segments": []map[string]any{
			{"id": 0, "start": 0.0, "end": 1.0, "text": text, "words": []any{}},
		},
	}
	b, _ := json.Marshal(doc)
	return b
}

// fakeCut writes a dummy clip file so ClipAndSplice's transcribe step proceeds; it
// never touches ffmpeg.
func fakeCut(_ context.Context, _, dstFlac string, _, _ float64) error {
	return os.WriteFile(dstFlac, []byte("flac"), 0o644)
}

func fakeTranscribe(text string) TranscribeClip {
	return func(_ context.Context, _ string) ([]byte, error) {
		return rawOpenAI(text), nil
	}
}

func locatedTranscript() transcript.Transcript {
	phrase := []string{"and", "then", "he", "ran", "away", "fast"}
	toks := loopTokens(60, phrase, 12, 100.0, 0.05)
	return buildTranscript(4, 8, toks)
}

func TestClipAndSplice_FabricatedSplices(t *testing.T) {
	dir := t.TempDir()
	req := ClipSpliceRequest{
		WorkDir:    dir,
		Chapter:    4,
		Transcript: locatedTranscript(),
		ChapterEnd: 200.0,
		Cut:        fakeCut,
		Transcribe: fakeTranscribe("he walked to the door and left the room quietly"),
	}
	res, err := ClipAndSplice(context.Background(), req)
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if !res.Located {
		t.Fatal("expected Located=true")
	}
	if res.Verdict != VerdictFabricated {
		t.Errorf("verdict = %s, want FABRICATED", res.Verdict)
	}
	if !res.Spliced {
		t.Fatal("expected Spliced=true")
	}
	// transcripts-repaired/ch004.txt written and does not contain the loop.
	repaired := filepath.Join(dir, transcript.RepairedDir, transcript.TextName(4))
	body, err := os.ReadFile(repaired)
	if err != nil {
		t.Fatalf("read repaired: %v", err)
	}
	if strings.Contains(string(body), "and then he ran away fast and then") {
		t.Error("repaired text still contains the loop")
	}
	if !strings.Contains(string(body), "walked to the door") {
		t.Error("repaired text missing the fresh clip ending")
	}
	// repairs.log has the entry.
	log := readFile(t, filepath.Join(dir, RepairsLogName))
	if !strings.Contains(log, "- ch004 [FABRICATED]") {
		t.Errorf("repairs.log missing entry:\n%s", log)
	}
	// tail_verdicts.json has the chapter.
	vs, err := LoadTailVerdicts(dir)
	if err != nil {
		t.Fatalf("LoadTailVerdicts: %v", err)
	}
	if len(vs) != 1 || vs[0].Chapter != 4 || vs[0].Verdict != VerdictFabricated {
		t.Errorf("tail_verdicts = %+v", vs)
	}
}

func TestClipAndSplice_RedegeneratedKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	req := ClipSpliceRequest{
		WorkDir:    dir,
		Chapter:    4,
		Transcript: locatedTranscript(),
		ChapterEnd: 200.0,
		Cut:        fakeCut,
		// The fresh clip loops too (unhealthy): keep the original, no splice.
		Transcribe: fakeTranscribe(strings.Repeat("and then he ran away fast ", 5)),
	}
	res, err := ClipAndSplice(context.Background(), req)
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if res.ClipHealthy {
		t.Error("expected the looping clip to be unhealthy")
	}
	if res.Verdict != VerdictClipRedegenerated {
		t.Errorf("verdict = %s, want CLIP-REDEGENERATED", res.Verdict)
	}
	if res.Spliced {
		t.Error("expected no splice on a re-degenerated clip")
	}
	if _, err := os.Stat(filepath.Join(dir, transcript.RepairedDir, transcript.TextName(4))); !os.IsNotExist(err) {
		t.Error("expected no repaired file")
	}
	if _, err := os.Stat(filepath.Join(dir, RepairsLogName)); !os.IsNotExist(err) {
		t.Error("expected no repairs.log entry")
	}
	// The verdict is still recorded.
	vs, _ := LoadTailVerdicts(dir)
	if len(vs) != 1 || vs[0].Verdict != VerdictClipRedegenerated {
		t.Errorf("tail_verdicts = %+v", vs)
	}
}

func TestClipAndSplice_NotLocatedNoOp(t *testing.T) {
	dir := t.TempDir()
	// A distinct-word transcript with no repeated 6-gram: nothing to locate.
	tr := buildTranscript(7, 8, loopTokens(60, nil, 0, 0, 0))
	req := ClipSpliceRequest{
		WorkDir:    dir,
		Chapter:    7,
		Transcript: tr,
		ChapterEnd: 30.0,
		Cut:        fakeCut,
		Transcribe: fakeTranscribe("unused"),
	}
	res, err := ClipAndSplice(context.Background(), req)
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if res.Located || res.Spliced {
		t.Errorf("expected no-op, got %+v", res)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("expected no artifacts written, got %d entries", len(entries))
	}
}

// recordingCut wraps fakeCut, capturing the window start it is called with (and the
// destination clip path) so a test can assert the effective window ClipAndSplice used
// and the filename it wrote.
type recordingCut struct {
	starts []float64
	dsts   []string
}

func (rc *recordingCut) cut(ctx context.Context, src, dst string, startSec, durSec float64) error {
	rc.starts = append(rc.starts, startSec)
	rc.dsts = append(rc.dsts, dst)
	return fakeCut(ctx, src, dst, startSec, durSec)
}

// TestClipAndSplice_StartOverrideHonored: a StartOverrideSec replaces the transcript-
// derived window start - the cut is taken from the override, and res.ClipStart reflects
// it (rounded) - while the splice/adjudication machinery still runs.
func TestClipAndSplice_StartOverrideHonored(t *testing.T) {
	dir := t.TempDir()
	rc := &recordingCut{}
	const override = 150.0
	req := ClipSpliceRequest{
		WorkDir:          dir,
		Chapter:          4,
		Transcript:       locatedTranscript(),
		ChapterEnd:       200.0,
		Cut:              rc.cut,
		Transcribe:       fakeTranscribe("he walked to the door and left the room quietly"),
		StartOverrideSec: override,
	}
	res, err := ClipAndSplice(context.Background(), req)
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if len(rc.starts) != 1 || rc.starts[0] != override {
		t.Fatalf("cut window starts = %v, want a single cut at %.1f (the override)", rc.starts, override)
	}
	if res.ClipStart != override {
		t.Errorf("res.ClipStart = %.1f, want %.1f (the override)", res.ClipStart, override)
	}
	if !res.Spliced {
		t.Error("expected the fabricated fresh clip to still splice under an override")
	}
}

// TestClipAndSplice_ClipFilenameStemIsDotFree guards the round-trip between the clip
// audio filename and the raw-output name the ASR backends derive from its stem: the
// backends split the extension in a way that treats a '.' in the stem as an extra
// suffix (mlx wrote t005-660.json for a t005-660.0.flac input), so a decimal window
// start in the name breaks the read-back. The name must carry exactly one '.', the
// extension - even for a fractional window start.
func TestClipAndSplice_ClipFilenameStemIsDotFree(t *testing.T) {
	dir := t.TempDir()
	rc := &recordingCut{}
	// A window start with a fractional part (0.1s resolution) is exactly the case that
	// produced a dotted filename before the deciseconds encoding.
	req := ClipSpliceRequest{
		WorkDir:          dir,
		Chapter:          5,
		Transcript:       locatedTranscript(),
		ChapterEnd:       200.0,
		Cut:              rc.cut,
		Transcribe:       fakeTranscribe("he walked to the door and left the room quietly"),
		StartOverrideSec: 150.5,
	}
	if _, err := ClipAndSplice(context.Background(), req); err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if len(rc.dsts) != 1 {
		t.Fatalf("expected one cut, got %d", len(rc.dsts))
	}
	base := filepath.Base(rc.dsts[0])
	if n := strings.Count(base, "."); n != 1 {
		t.Errorf("clip filename %q has %d dots, want exactly 1 (the extension) - a dotted stem breaks the ASR raw-output round-trip", base, n)
	}
	if !strings.HasSuffix(base, ".flac") {
		t.Errorf("clip filename %q is not a .flac", base)
	}
}

// recordingTranscribe counts how many times it is invoked (0 proves the known-failed
// skip cut no audio and ran no ASR).
type recordingTranscribe struct {
	calls int
}

func (rt *recordingTranscribe) fn(_ context.Context, _ string) ([]byte, error) {
	rt.calls++
	return rawOpenAI("he walked to the door and left the room quietly"), nil
}

// TestClipAndSplice_KnownFailedWindowSkipped is the item-2 regression: when the effective
// window matches a prior CLIP-REDEGENERATED verdict for the chapter AND the decode tags
// match, ClipAndSplice skips the cut+transcribe entirely (no ASR, ~minutes saved) and
// returns SkippedKnownFailed.
func TestClipAndSplice_KnownFailedWindowSkipped(t *testing.T) {
	dir := t.TempDir()
	const override = 150.0
	const tag = "nocontext-v1"
	// Seed the prior round's verdict: this exact window already re-degenerated UNDER THE
	// SAME decode params (tag).
	if err := MergeTailVerdict(dir, TailVerdict{Chapter: 4, ClipStart: override, Verdict: VerdictClipRedegenerated, DecodeTag: tag}); err != nil {
		t.Fatal(err)
	}
	rc := &recordingCut{}
	rt := &recordingTranscribe{}
	res, err := ClipAndSplice(context.Background(), ClipSpliceRequest{
		WorkDir:          dir,
		Chapter:          4,
		Transcript:       locatedTranscript(),
		ChapterEnd:       200.0,
		Cut:              rc.cut,
		Transcribe:       rt.fn,
		StartOverrideSec: override,
		DecodeTag:        tag,
	})
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if !res.SkippedKnownFailed {
		t.Error("expected SkippedKnownFailed=true for a known-failed window")
	}
	if res.Spliced {
		t.Error("expected no splice on a known-failed skip")
	}
	if res.Verdict != VerdictClipRedegenerated {
		t.Errorf("verdict = %s, want CLIP-REDEGENERATED", res.Verdict)
	}
	if rt.calls != 0 {
		t.Errorf("Transcribe called %d times, want 0 (known-failed skip must not re-transcribe)", rt.calls)
	}
	if len(rc.starts) != 0 {
		t.Errorf("Cut called %d times, want 0 (known-failed skip must not re-cut)", len(rc.starts))
	}
}

// TestClipAndSplice_LegacyVerdictNotSkipped is the item-2 legacy-tag regression: a
// CLIP-REDEGENERATED verdict written BEFORE decode tags existed (empty DecodeTag) was
// produced under context-conditioned decoding, so it must NOT block a fresh NoContext
// attempt (a differing tag never matches) - the chapter gets exactly one re-transcription
// under the new params, whose new verdict then re-arms the skip.
func TestClipAndSplice_LegacyVerdictNotSkipped(t *testing.T) {
	dir := t.TempDir()
	const override = 150.0
	// A legacy verdict at the SAME window but with an empty (pre-tag) DecodeTag.
	if err := MergeTailVerdict(dir, TailVerdict{Chapter: 4, ClipStart: override, Verdict: VerdictClipRedegenerated}); err != nil {
		t.Fatal(err)
	}
	rc := &recordingCut{}
	rt := &recordingTranscribe{}
	res, err := ClipAndSplice(context.Background(), ClipSpliceRequest{
		WorkDir:          dir,
		Chapter:          4,
		Transcript:       locatedTranscript(),
		ChapterEnd:       200.0,
		Cut:              rc.cut,
		Transcribe:       rt.fn,
		StartOverrideSec: override,
		DecodeTag:        "nocontext-v1", // differs from the legacy verdict's empty tag
	})
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if res.SkippedKnownFailed {
		t.Error("a legacy (empty-tag) verdict must NOT skip a fresh NoContext attempt")
	}
	if rt.calls != 1 {
		t.Errorf("Transcribe called %d times, want 1 (one fresh attempt under the new params)", rt.calls)
	}
	if len(rc.starts) != 1 {
		t.Errorf("Cut called %d times, want 1 (the window is re-cut for the fresh attempt)", len(rc.starts))
	}
}

// TestClipAndSplice_NearButDistinctWindowNotSkipped: a prior CLIP-REDEGENERATED verdict
// more than the tolerance away from the effective window does NOT trigger the skip - a
// genuinely relocated (agent-supplied) window is re-attempted.
func TestClipAndSplice_NearButDistinctWindowNotSkipped(t *testing.T) {
	dir := t.TempDir()
	// Prior failure at 150.0; the agent relocates to 165.0 (> 1s away) -> not skipped.
	if err := MergeTailVerdict(dir, TailVerdict{Chapter: 4, ClipStart: 150.0, Verdict: VerdictClipRedegenerated}); err != nil {
		t.Fatal(err)
	}
	rt := &recordingTranscribe{}
	res, err := ClipAndSplice(context.Background(), ClipSpliceRequest{
		WorkDir:          dir,
		Chapter:          4,
		Transcript:       locatedTranscript(),
		ChapterEnd:       200.0,
		Cut:              fakeCut,
		Transcribe:       rt.fn,
		StartOverrideSec: 165.0,
	})
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if res.SkippedKnownFailed {
		t.Error("a distinct relocated window must not be skipped as known-failed")
	}
	if rt.calls != 1 {
		t.Errorf("Transcribe called %d times, want 1 (the relocated window is re-attempted)", rt.calls)
	}
}

// --- AppendRepairLog / MergeTailVerdict -------------------------------------

func TestAppendRepairLog_HeaderThenAppend(t *testing.T) {
	dir := t.TempDir()
	if err := AppendRepairLog(dir, "- ch001 [BENIGN]: x"); err != nil {
		t.Fatal(err)
	}
	if err := AppendRepairLog(dir, "- ch002 [FABRICATED]: y"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(dir, RepairsLogName))
	want := repairsLogHeader + "\n\n- ch001 [BENIGN]: x\n- ch002 [FABRICATED]: y\n"
	if got != want {
		t.Errorf("repairs.log =\n%q\nwant\n%q", got, want)
	}
}

func TestMergeTailVerdict_UpsertsAndSorts(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(MergeTailVerdict(dir, TailVerdict{Chapter: 5, Verdict: VerdictBenign, InClip: 1}))
	must(MergeTailVerdict(dir, TailVerdict{Chapter: 2, Verdict: VerdictFabricated}))
	// Re-entry replaces ch5's verdict.
	must(MergeTailVerdict(dir, TailVerdict{Chapter: 5, Verdict: VerdictClipRedegenerated, InClip: 4}))

	vs, err := LoadTailVerdicts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 {
		t.Fatalf("len = %d, want 2 (%+v)", len(vs), vs)
	}
	if vs[0].Chapter != 2 || vs[1].Chapter != 5 {
		t.Errorf("not sorted by chapter: %+v", vs)
	}
	if vs[1].Verdict != VerdictClipRedegenerated || vs[1].InClip != 4 {
		t.Errorf("ch5 not upserted: %+v", vs[1])
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
