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

// --- mid_clip (ClipAndSpliceWindow / SpliceWindow) --------------------------

// midSegTranscript builds a 4-segment chapter (A[0-10], B[10-20], C[20-30], D[30-40])
// whose interior segments (B, C) carry a garbage loop and whose edges (A, D) are clean.
func midSegTranscript(chapter int) transcript.Transcript {
	return transcript.Transcript{
		Schema: transcript.Schema, Chapter: chapter,
		Segments: []transcript.Segment{
			{ID: 0, Start: 0, End: 10, Text: " alpha one two"},
			{ID: 1, Start: 10, End: 20, Text: " loop the loop the loop the"},
			{ID: 2, Start: 20, End: 30, Text: " loop the loop the loop the"},
			{ID: 3, Start: 30, End: 40, Text: " delta seven eight"},
		},
	}
}

// TestSpliceWindow_SnapsHeadWindowTail: a window [12, 28] straddling segments B and C
// snaps OUTWARD to segment edges [10, 30], so the head is A (End <= 10), the tail is D
// (Start >= 30), and the fresh window text replaces B+C - no straddling content lost, no
// seam duplication. The word counts mirror Splice (len(text.split())).
func TestSpliceWindow_SnapsHeadWindowTail(t *testing.T) {
	tr := midSegTranscript(4)
	text, before, after := SpliceWindow(tr, 12, 28, "he walked to the door quietly")
	want := "alpha one two he walked to the door quietly delta seven eight"
	if text != want {
		t.Errorf("SpliceWindow text = %q, want %q", text, want)
	}
	if strings.Contains(text, "loop the loop") {
		t.Error("the interior loop segments were not dropped by the window")
	}
	// before = words in the original (A 3 + B 6 + C 6 + D 3 = 18); after = head A 3 +
	// window 6 + tail D 3 = 12.
	if before != 18 {
		t.Errorf("wordsBefore = %d, want 18", before)
	}
	if after != 12 {
		t.Errorf("wordsAfter = %d, want 12", after)
	}
}

// TestSpliceWindow_EmptyHeadCollapses: a window starting at the chapter head (before any
// segment ends) yields an empty head, and the join collapses the leading gap cleanly.
func TestSpliceWindow_EmptyHeadCollapses(t *testing.T) {
	tr := midSegTranscript(4)
	// [0, 28] -> snappedStart stays 0 (no segment ends at or before 0), snappedEnd 30:
	// head empty, tail = D, window replaces A+B+C.
	text, _, _ := SpliceWindow(tr, 0, 28, "fresh replacement narration here")
	if strings.HasPrefix(text, " ") {
		t.Errorf("text has a leading space (not collapsed): %q", text)
	}
	if text != "fresh replacement narration here delta seven eight" {
		t.Errorf("text = %q", text)
	}
}

// TestClipAndSpliceWindow_HealthySplices: a healthy fresh window is spliced between the
// intact head and tail; the recorded verdict is MID-REPAIRED with BOTH bounds set (a mid
// window), the clip filename is dot-free with the m-prefix, and repairs.log gets an entry.
func TestClipAndSpliceWindow_HealthySplices(t *testing.T) {
	dir := t.TempDir()
	rc := &recordingCut{}
	res, err := ClipAndSpliceWindow(context.Background(), ClipSpliceRequest{
		WorkDir:          dir,
		Chapter:          4,
		Transcript:       midSegTranscript(4),
		Cut:              rc.cut,
		Transcribe:       fakeTranscribe("he walked to the door and left the room quietly"),
		StartOverrideSec: 12,
		EndOverrideSec:   28,
		DecodeTag:        "nocontext-v1",
	})
	if err != nil {
		t.Fatalf("ClipAndSpliceWindow: %v", err)
	}
	if !res.Spliced || res.Verdict != VerdictMidRepaired {
		t.Fatalf("res = %+v, want a MID-REPAIRED splice", res)
	}
	// The window snapped to [10, 30]: one cut at 10 for 20s.
	if len(rc.starts) != 1 || rc.starts[0] != 10 {
		t.Fatalf("cut window starts = %v, want [10] (snapped from 12)", rc.starts)
	}
	if res.ClipStart != 10 {
		t.Errorf("res.ClipStart = %.1f, want 10 (snapped)", res.ClipStart)
	}
	// The repaired text has the head + fresh window + tail, and no loop.
	body := readFile(t, filepath.Join(dir, transcript.RepairedDir, transcript.TextName(4)))
	if strings.Contains(body, "loop the loop") {
		t.Error("repaired text still contains the interior loop")
	}
	if !strings.Contains(body, "walked to the door") || !strings.Contains(body, "alpha one two") || !strings.Contains(body, "delta seven eight") {
		t.Errorf("repaired text missing head/window/tail: %q", body)
	}
	// The verdict records BOTH bounds (a mid window) - ClipEnd is what the residual
	// auto-accept keys on.
	vs, err := LoadTailVerdicts(dir)
	if err != nil || len(vs) != 1 {
		t.Fatalf("verdicts = %+v (%v)", vs, err)
	}
	if vs[0].ClipStart != 10 || vs[0].ClipEnd != 30 || vs[0].Verdict != VerdictMidRepaired {
		t.Errorf("verdict = %+v, want ClipStart 10 ClipEnd 30 MID-REPAIRED", vs[0])
	}
	// The clip stem must be dot-free (the ASR raw-output round-trip) with the m-prefix and
	// both bounds in deciseconds.
	base := filepath.Base(rc.dsts[0])
	if n := strings.Count(base, "."); n != 1 {
		t.Errorf("clip filename %q has %d dots, want exactly 1 (the extension)", base, n)
	}
	if base != "m004-100-300.flac" {
		t.Errorf("clip filename = %q, want m004-100-300.flac (m-prefix, deciseconds bounds)", base)
	}
	log := readFile(t, filepath.Join(dir, RepairsLogName))
	if !strings.Contains(log, "- ch004 [MID-REPAIRED]: spliced mid-window") {
		t.Errorf("repairs.log missing mid entry:\n%s", log)
	}
}

// TestClipAndSpliceWindow_RedegeneratedKeepsOriginal: an unhealthy fresh window (it loops
// too) records a mid CLIP-REDEGENERATED verdict (with ClipEnd) and keeps the original - no
// repaired file, no repairs.log entry.
func TestClipAndSpliceWindow_RedegeneratedKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	res, err := ClipAndSpliceWindow(context.Background(), ClipSpliceRequest{
		WorkDir:          dir,
		Chapter:          4,
		Transcript:       midSegTranscript(4),
		Cut:              fakeCut,
		Transcribe:       fakeTranscribe(strings.Repeat("and then he ran away fast ", 5)),
		StartOverrideSec: 12,
		EndOverrideSec:   28,
		DecodeTag:        "nocontext-v1",
	})
	if err != nil {
		t.Fatalf("ClipAndSpliceWindow: %v", err)
	}
	if res.ClipHealthy {
		t.Error("expected the looping fresh window to be unhealthy")
	}
	if res.Spliced || res.Verdict != VerdictClipRedegenerated {
		t.Errorf("res = %+v, want no splice + CLIP-REDEGENERATED", res)
	}
	if _, err := os.Stat(filepath.Join(dir, transcript.RepairedDir, transcript.TextName(4))); !os.IsNotExist(err) {
		t.Error("expected no repaired file on a re-degenerated mid window")
	}
	if _, err := os.Stat(filepath.Join(dir, RepairsLogName)); !os.IsNotExist(err) {
		t.Error("expected no repairs.log entry on a re-degenerated mid window")
	}
	vs, _ := LoadTailVerdicts(dir)
	if len(vs) != 1 || vs[0].ClipEnd != 30 || vs[0].Verdict != VerdictClipRedegenerated {
		t.Errorf("verdict = %+v, want a mid CLIP-REDEGENERATED with ClipEnd 30", vs)
	}
}

// TestClipAndSpliceWindow_KnownFailedMidSkipped: a mid window matching a prior mid
// CLIP-REDEGENERATED verdict (BOTH bounds within tolerance, same decode tag) is skipped -
// no cut, no re-transcribe - and returns SkippedKnownFailed.
func TestClipAndSpliceWindow_KnownFailedMidSkipped(t *testing.T) {
	dir := t.TempDir()
	const tag = "nocontext-v1"
	// The prior round's mid failure at the SAME snapped window [10, 30] under the same tag.
	if err := MergeTailVerdict(dir, TailVerdict{Chapter: 4, ClipStart: 10, ClipEnd: 30, Verdict: VerdictClipRedegenerated, DecodeTag: tag}); err != nil {
		t.Fatal(err)
	}
	rc := &recordingCut{}
	rt := &recordingTranscribe{}
	res, err := ClipAndSpliceWindow(context.Background(), ClipSpliceRequest{
		WorkDir:          dir,
		Chapter:          4,
		Transcript:       midSegTranscript(4),
		Cut:              rc.cut,
		Transcribe:       rt.fn,
		StartOverrideSec: 12, // snaps to 10 == the recorded window
		EndOverrideSec:   28, // snaps to 30 == the recorded window
		DecodeTag:        tag,
	})
	if err != nil {
		t.Fatalf("ClipAndSpliceWindow: %v", err)
	}
	if !res.SkippedKnownFailed {
		t.Error("expected SkippedKnownFailed for a known-failed mid window")
	}
	if res.Spliced {
		t.Error("expected no splice on a known-failed mid skip")
	}
	if rt.calls != 0 {
		t.Errorf("Transcribe called %d times, want 0 (known-failed mid skip must not re-transcribe)", rt.calls)
	}
	if len(rc.starts) != 0 {
		t.Errorf("Cut called %d times, want 0 (known-failed mid skip must not re-cut)", len(rc.starts))
	}
}

// TestClipAndSpliceWindow_DistinctMidWindowNotSkipped: a prior mid verdict whose END is far
// from the requested window's end does NOT trigger the skip (both bounds must match), so a
// genuinely relocated interior window is re-attempted.
func TestClipAndSpliceWindow_DistinctMidWindowNotSkipped(t *testing.T) {
	dir := t.TempDir()
	const tag = "nocontext-v1"
	// Same start (10) but a very different end (60) - a distinct window, must not skip.
	if err := MergeTailVerdict(dir, TailVerdict{Chapter: 4, ClipStart: 10, ClipEnd: 60, Verdict: VerdictClipRedegenerated, DecodeTag: tag}); err != nil {
		t.Fatal(err)
	}
	rt := &recordingTranscribe{}
	res, err := ClipAndSpliceWindow(context.Background(), ClipSpliceRequest{
		WorkDir:          dir,
		Chapter:          4,
		Transcript:       midSegTranscript(4),
		Cut:              fakeCut,
		Transcribe:       rt.fn,
		StartOverrideSec: 12, // snaps to 10, but the recorded end (60) differs from 30
		EndOverrideSec:   28,
		DecodeTag:        tag,
	})
	if err != nil {
		t.Fatalf("ClipAndSpliceWindow: %v", err)
	}
	if res.SkippedKnownFailed {
		t.Error("a distinct mid window (different end) must not be skipped as known-failed")
	}
	if rt.calls != 1 {
		t.Errorf("Transcribe called %d times, want 1 (the distinct window is re-attempted)", rt.calls)
	}
}

// TestClipAndSpliceWindow_InvalidWindow: a non-positive start or an end not past the start
// is a loud error (Validate guards this upstream, but the repair layer defends too).
func TestClipAndSpliceWindow_InvalidWindow(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ start, end float64 }{{0, 10}, {20, 10}, {20, 20}} {
		_, err := ClipAndSpliceWindow(context.Background(), ClipSpliceRequest{
			WorkDir: dir, Chapter: 4, Transcript: midSegTranscript(4),
			Cut: fakeCut, Transcribe: fakeTranscribe("x"),
			StartOverrideSec: tc.start, EndOverrideSec: tc.end,
		})
		if err == nil {
			t.Errorf("window [%g, %g] should be rejected", tc.start, tc.end)
		}
	}
}

// TestClipAndSplice_MidVerdictDoesNotBlockTail is the ClipEnd==0-guard regression: a mid
// CLIP-REDEGENERATED verdict (ClipEnd > 0) must NOT satisfy the TAIL known-failed skip, so
// a tail clip at the same start still runs its one fresh attempt.
func TestClipAndSplice_MidVerdictDoesNotBlockTail(t *testing.T) {
	dir := t.TempDir()
	const override = 150.0
	const tag = "nocontext-v1"
	// A MID verdict at the same clip_start (but with a ClipEnd) - a tail request must ignore it.
	if err := MergeTailVerdict(dir, TailVerdict{Chapter: 4, ClipStart: override, ClipEnd: 175, Verdict: VerdictClipRedegenerated, DecodeTag: tag}); err != nil {
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
		StartOverrideSec: override,
		DecodeTag:        tag,
	})
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if res.SkippedKnownFailed {
		t.Error("a MID verdict (ClipEnd > 0) must not block a tail clip via the known-failed skip")
	}
	if rt.calls != 1 {
		t.Errorf("Transcribe called %d times, want 1 (the tail window runs its fresh attempt)", rt.calls)
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

// TestTailVerdictIsMidWindow pins the single ClipEnd-sentinel reading the known-failed
// skips and the residual auto-accept share: a mid verdict carries a bounded window
// (ClipEnd > 0), a tail verdict runs to the chapter end (ClipEnd == 0).
func TestTailVerdictIsMidWindow(t *testing.T) {
	if (TailVerdict{ClipStart: 10, ClipEnd: 30}).IsMidWindow() != true {
		t.Error("a verdict with ClipEnd > 0 must be a mid window")
	}
	if (TailVerdict{ClipStart: 10}).IsMidWindow() != false {
		t.Error("a verdict with ClipEnd == 0 must be a tail window")
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
