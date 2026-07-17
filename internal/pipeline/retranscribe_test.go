package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/repair"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// seedPlan writes qa_plan.json with the given entries (production writes it via
// agent.Harvest, so the test emits the artifact directly).
func seedPlan(t *testing.T, work string, entries ...qa.PlanEntry) {
	t.Helper()
	out, err := json.MarshalIndent(&qa.Plan{Entries: entries}, "", "  ")
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, qa.PlanFile), append(out, '\n'), 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}
}

// oneWordTranscript is a deliberately collapsed original: 1 word over the chapter, so
// its word rate is implausibly low (the retranscribe adoption check should replace it
// when a plausible fresh run appears).
func oneWordTranscript(chapter int) transcript.Transcript {
	return transcript.Transcript{
		Schema: transcript.Schema, Chapter: chapter,
		Segments: []transcript.Segment{{ID: 0, Start: 0, End: 1, Text: " word"}},
	}
}

// TestRetranscribeEmitsStageEntryNote: retranscribing emits a descriptive stage-entry
// note naming the chapters it will re-transcribe (the non-accept plan entries), and
// excludes accepted chapters.
func TestRetranscribeEmitsStageEntryNote(t *testing.T) {
	work := t.TempDir()
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 1, ChapterCount: 3, Chapters: []audio.Chapter{
		{Chapter: 1, Start: 0, End: 1, Duration: 1},
		{Chapter: 2, Start: 1, End: 2, Duration: 1},
		{Chapter: 3, Start: 2, End: 3, Duration: 1},
	}})
	seedNormalized(t, work, oneWordTranscript(2))
	seedNormalized(t, work, oneWordTranscript(3))
	seedFLACs(t, work, 3)
	seedPlan(t, work,
		qa.PlanEntry{Chapter: 1, Action: qa.ActionAccept, Reason: "fine"},
		qa.PlanEntry{Chapter: 2, Action: qa.ActionRetranscribe, Reason: "collapse"},
		qa.PlanEntry{Chapter: 3, Action: qa.ActionRetranscribe, Reason: "collapse"},
	)

	var notes []string
	rep := scheduler.StageReport{Note: func(msg string) { notes = append(notes, msg) }}

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, rep); err != nil {
		t.Fatalf("retranscribing: %v", err)
	}

	want := "re-transcribing 2 chapters: 2, 3"
	found := false
	for _, n := range notes {
		if n == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("stage-entry note %q not emitted; got %v", want, notes)
	}
}

// TestRetranscribeAdoptsPlausibleFresh: a collapsed original (1 word / 1s) is replaced
// by a plausible fresh run (the fake backend's 3-word transcript / 1s), the raw is
// re-frozen 0444, and the derived layers are re-derived.
func TestRetranscribeAdoptsPlausibleFresh(t *testing.T) {
	work := t.TempDir()
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 1, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: 1, Duration: 1}}})
	seedNormalized(t, work, oneWordTranscript(2))
	seedFLACs(t, work, 2) // ch001+ch002 placeholder FLACs (fake backend ignores content)
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionRetranscribe, Reason: "collapse"})

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"retranscribed": 1, "adopted": 1, "kept": 0})
	// The normalized layer now carries the fresh text, and the raw is re-frozen.
	txt, err := os.ReadFile(filepath.Join(work, transcript.TextDir, transcript.TextName(2)))
	if err != nil || !strings.Contains(string(txt), "fake chapter 2") {
		t.Errorf("text layer not re-derived from the fresh run: %q (%v)", txt, err)
	}
	info, err := os.Stat(filepath.Join(work, transcript.RawDir, transcript.RawName(2)))
	if err != nil {
		t.Fatalf("replaced raw missing: %v", err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Errorf("replaced raw perm = %o, want 444 (re-frozen)", info.Mode().Perm())
	}
	if fake.count(2) != 1 {
		t.Errorf("chapter 2 transcribed %d times, want 1", fake.count(2))
	}
	// The repair re-transcription must disable context-conditioning so a deterministic
	// repetition collapse does not replay identically on the retry.
	if !fake.sawNoContext(2) {
		t.Error("retranscribeChapter did not set NoContext on the re-transcription Job")
	}
}

// TestRetranscribeKeepsCollapsedFresh: when the fresh run collapses (implausible for the
// chapter duration), the original is kept untouched.
func TestRetranscribeKeepsCollapsedFresh(t *testing.T) {
	work := t.TempDir()
	// A 60s chapter: the fake backend's 3-word fresh run is implausibly slow -> keep.
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 60, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: 60, Duration: 60}}})
	orig := transcript.Transcript{Schema: transcript.Schema, Chapter: 2, Segments: []transcript.Segment{{ID: 0, Start: 0, End: 60, Text: " ORIGINAL KEEP ME"}}}
	seedNormalized(t, work, orig)
	seedFLACs(t, work, 2)
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionRetranscribe, Reason: "check"})

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"retranscribed": 1, "adopted": 0, "kept": 1})
	// The original normalized transcript survives (fresh was not adopted).
	got, err := transcript.ReadNormalized(filepath.Join(work, transcript.JSONDir), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transcript.PlainText(got), "ORIGINAL KEEP ME") {
		t.Errorf("original transcript was replaced by a collapsed fresh run: %q", transcript.PlainText(got))
	}
}

// TestRetranscribeTailClipSplices: a chapter with a locatable tail loop is tail-clipped -
// the window is cut (fake cutter), re-transcribed prompt-free (fake backend), and
// spliced, writing transcripts-repaired and a repairs.log line.
func TestRetranscribeTailClipSplices(t *testing.T) {
	work := t.TempDir()
	loop, dur := tailLoopTranscript(2)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: dur, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: dur, Duration: dur}}})
	seedNormalized(t, work, loop)
	seedFLACs(t, work, 2)
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionTailClip, Reason: "tail loop"})

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	// A fake cutter writes a placeholder clip so no real ffmpeg is needed.
	exe.clipCutter = func(_ context.Context, _, dstFlac string, _, _ float64) error {
		return os.WriteFile(dstFlac, []byte("clip"), 0o644) //nolint:gosec // test artifact
	}
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"clips_spliced": 1, "clips_redegenerated": 0})
	if _, err := os.Stat(filepath.Join(work, transcript.RepairedDir, transcript.TextName(2))); err != nil {
		t.Errorf("transcripts-repaired/ch002.txt missing after splice: %v", err)
	}
	logRaw, err := os.ReadFile(filepath.Join(work, repair.RepairsLogName))
	if err != nil {
		t.Fatalf("repairs.log missing: %v", err)
	}
	if !strings.Contains(string(logRaw), "ch002") {
		t.Errorf("repairs.log has no ch002 entry:\n%s", logRaw)
	}
	// The clip re-transcription must disable context-conditioning - the tail loop being
	// cut is a context-conditioned collapse, so a context-on retry would just replay it.
	if !fake.sawNoContext(2) {
		t.Error("tailClipChapter did not set NoContext on the clip re-transcription Job")
	}
}

// midSegLoopTranscript builds a 4-segment chapter (A[0-10], B[10-20], C[20-30], D[30-40])
// with an interior loop in B+C and clean edges A/D, so a mid_clip window [12,28] snaps to
// [10,30] and splices the fresh window between the intact head and tail.
func midSegLoopTranscript(chapter int) (transcript.Transcript, float64) {
	return transcript.Transcript{
		Schema: transcript.Schema, Chapter: chapter,
		Segments: []transcript.Segment{
			{ID: 0, Start: 0, End: 10, Text: " alpha one two"},
			{ID: 1, Start: 10, End: 20, Text: " loop the loop the loop the"},
			{ID: 2, Start: 20, End: 30, Text: " loop the loop the loop the"},
			{ID: 3, Start: 30, End: 40, Text: " delta seven eight"},
		},
	}, 40
}

// TestRetranscribeMidClipSplices: a chapter with an INTERIOR degeneration loop is mid-
// clipped - the agent-supplied [clip_start_sec, clip_end_sec] window is cut (fake cutter),
// re-transcribed prompt-free (NoContext), and spliced between the intact head and tail,
// writing transcripts-repaired and a mid repairs.log line. It counts under clips_spliced
// (the same bucket as a tail splice), so it feeds the stall/progress convergence signal.
func TestRetranscribeMidClipSplices(t *testing.T) {
	work := t.TempDir()
	tr, dur := midSegLoopTranscript(2)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: dur, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: dur, Duration: dur}}})
	seedNormalized(t, work, tr)
	seedFLACs(t, work, 2)
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionMidClip, Reason: "interior loop", ClipStartSec: 12, ClipEndSec: 28})

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	exe.clipCutter = func(_ context.Context, _, dstFlac string, _, _ float64) error {
		return os.WriteFile(dstFlac, []byte("clip"), 0o644) //nolint:gosec // test artifact
	}
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"clips_spliced": 1, "clips_redegenerated": 0})
	body, err := os.ReadFile(filepath.Join(work, transcript.RepairedDir, transcript.TextName(2)))
	if err != nil {
		t.Fatalf("transcripts-repaired/ch002.txt missing after mid splice: %v", err)
	}
	if strings.Contains(string(body), "loop the loop") {
		t.Errorf("repaired text still contains the interior loop: %q", body)
	}
	if !strings.Contains(string(body), "fake chapter 2") {
		t.Errorf("repaired text missing the fresh window: %q", body)
	}
	// The mid re-transcription must disable context-conditioning (same rationale as tail).
	if !fake.sawNoContext(2) {
		t.Error("midClipChapter did not set NoContext on the clip re-transcription Job")
	}
	// The verdict records a mid window (ClipEnd set), which the residual auto-accept keys on.
	vs, err := repair.LoadTailVerdicts(work)
	if err != nil || len(vs) != 1 {
		t.Fatalf("verdicts = %+v (%v)", vs, err)
	}
	if vs[0].ClipEnd == 0 || vs[0].Verdict != repair.VerdictMidRepaired {
		t.Errorf("verdict = %+v, want a mid MID-REPAIRED with ClipEnd set", vs[0])
	}
	log, err := os.ReadFile(filepath.Join(work, repair.RepairsLogName))
	if err != nil || !strings.Contains(string(log), "ch002 [MID-REPAIRED]") {
		t.Errorf("repairs.log missing the mid ch002 entry: %s (%v)", log, err)
	}
}

// TestRetranscribeMidClipClampsEndToChapter: an agent clip_end_sec past the chapter end is
// clamped so the cut never runs past EOF (which would empty the tail and silently degrade
// the interior splice into a tail-to-EOF one).
func TestRetranscribeMidClipClampsEndToChapter(t *testing.T) {
	work := t.TempDir()
	tr, dur := midSegLoopTranscript(2)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: dur, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: dur, Duration: dur}}})
	seedNormalized(t, work, tr)
	seedFLACs(t, work, 2)
	// clip_end_sec well past the chapter duration.
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionMidClip, Reason: "interior loop", ClipStartSec: 12, ClipEndSec: dur + 500})

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	var cutStart, cutDur float64
	exe.clipCutter = func(_ context.Context, _, dstFlac string, startSec, durSec float64) error {
		cutStart, cutDur = startSec, durSec
		return os.WriteFile(dstFlac, []byte("clip"), 0o644) //nolint:gosec // test artifact
	}
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{}); err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	if cutEnd := cutStart + cutDur; cutEnd > dur+0.01 {
		t.Errorf("cut window end %.1f exceeds chapter duration %.1f (endSec not clamped)", cutEnd, dur)
	}
}

// TestRetranscribeMidClipStallMarkerClearedOnProgress: a mid splice makes progress
// (spliced > 0), so it clears any stale stall marker - a mid repair converges the QA loop
// exactly like a tail splice.
func TestRetranscribeMidClipStallMarkerClearedOnProgress(t *testing.T) {
	work := t.TempDir()
	tr, dur := midSegLoopTranscript(2)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: dur, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: dur, Duration: dur}}})
	seedNormalized(t, work, tr)
	seedFLACs(t, work, 2)
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionMidClip, Reason: "interior loop", ClipStartSec: 12, ClipEndSec: 28})
	if err := os.WriteFile(filepath.Join(work, retranscribeStalledMarker), []byte("1\n"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	exe.clipCutter = func(_ context.Context, _, dstFlac string, _, _ float64) error {
		return os.WriteFile(dstFlac, []byte("clip"), 0o644) //nolint:gosec // test artifact
	}
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"clips_spliced": 1})
	if fileExistsT(filepath.Join(work, retranscribeStalledMarker)) {
		t.Error("a mid splice (progress) did not clear the stall marker")
	}
}

// seedFreshRaw writes a structurally-complete fresh raw into a retranscribe/ dir, as if a
// prior run had produced it (used to exercise the decode-params marker's stale-raw purge).
func seedFreshRaw(t *testing.T, dir string, chapter int) {
	t.Helper()
	raw := fmt.Sprintf(`{"text":" fake chapter %d","language":"en","segments":[{"id":0,"start":0,"end":1,"text":" fake chapter %d","words":[{"word":" fake","start":0,"end":0.5,"probability":0.9}]}]}`, chapter, chapter)
	if err := os.WriteFile(filepath.Join(dir, transcript.RawName(chapter)), []byte(raw+"\n"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

// TestRetranscribeDecodeParamsMarker is the item-3 regression: a structurally-complete
// fresh raw left by a PRE-NoContext run must be discarded (so the chapter is re-transcribed
// under the new NoContext params) when the retranscribe/ decode-params marker is absent or
// records different params, while a raw whose marker matches the current tag is reused (the
// intended cheap resume).
func TestRetranscribeDecodeParamsMarker(t *testing.T) {
	for _, tc := range []struct {
		name      string
		marker    string // "" = no marker file at all
		wantCalls int
	}{
		{"missing marker re-transcribes", "", 1},
		{"mismatched marker re-transcribes", "old-params", 1},
		{"matching marker reuses", retranscribeDecodeTag, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 1, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: 1, Duration: 1}}})
			seedNormalized(t, work, oneWordTranscript(2))
			seedFLACs(t, work, 2)
			seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionRetranscribe, Reason: "collapse"})
			// Pre-seed a complete fresh raw (as if an earlier run produced it) + optional marker.
			freshDir := filepath.Join(work, repair.RetranscribeDir)
			if err := os.MkdirAll(freshDir, 0o750); err != nil {
				t.Fatal(err)
			}
			seedFreshRaw(t, freshDir, 2)
			if tc.marker != "" {
				if err := os.WriteFile(filepath.Join(freshDir, "decode_params"), []byte(tc.marker+"\n"), 0o644); err != nil { //nolint:gosec // test artifact
					t.Fatal(err)
				}
			}

			fake := newFakeBackend()
			exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
			if _, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{}); err != nil {
				t.Fatalf("retranscribing: %v", err)
			}
			if fake.count(2) != tc.wantCalls {
				t.Errorf("chapter 2 transcribed %d times, want %d", fake.count(2), tc.wantCalls)
			}
			// After the stage the marker always records the current tag.
			got, err := os.ReadFile(filepath.Join(freshDir, "decode_params"))
			if err != nil || strings.TrimSpace(string(got)) != retranscribeDecodeTag {
				t.Errorf("decode-params marker = %q (%v), want %q", got, err, retranscribeDecodeTag)
			}
		})
	}
}

// TestRetranscribeTailClipKnownFailedSkipped is the item-2/C regression: a tail_clip
// entry whose effective window already carries a CLIP-REDEGENERATED verdict is skipped -
// no re-cut, no re-ASR - and counted under clips_skipped_known_failed (distinct from a
// fresh re-degeneration). The agent re-queues the same window via clip_start_sec.
func TestRetranscribeTailClipKnownFailedSkipped(t *testing.T) {
	work := t.TempDir()
	loop, dur := tailLoopTranscript(2)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: dur, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: dur, Duration: dur}}})
	seedNormalized(t, work, loop)
	seedFLACs(t, work, 2)
	// A prior round already cut this window and it re-degenerated UNDER THE CURRENT decode
	// params (verdict only, no splice; the tag matches what the stage passes today).
	const window = 10.0
	if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: 2, ClipStart: window, Verdict: repair.VerdictClipRedegenerated, DecodeTag: retranscribeDecodeTag}); err != nil {
		t.Fatal(err)
	}
	// The agent re-queues the SAME window via clip_start_sec.
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionTailClip, Reason: "tail loop", ClipStartSec: window})

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	cutCalls := 0
	exe.clipCutter = func(_ context.Context, _, dstFlac string, _, _ float64) error {
		cutCalls++
		return os.WriteFile(dstFlac, []byte("clip"), 0o644) //nolint:gosec // test artifact
	}
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"clips_skipped_known_failed": 1, "clips_spliced": 0, "clips_redegenerated": 0})
	if fake.count(2) != 0 {
		t.Errorf("a known-failed window re-transcribed chapter 2 %d times, want 0", fake.count(2))
	}
	if cutCalls != 0 {
		t.Errorf("a known-failed window was cut %d times, want 0", cutCalls)
	}
	if _, err := os.Stat(filepath.Join(work, transcript.RepairedDir, transcript.TextName(2))); !os.IsNotExist(err) {
		t.Errorf("known-failed skip must not write a repaired file, stat err = %v", err)
	}
	// Item-6: a free known-failed skip did no productive ASR, so it must record no rate
	// sample (a skip-only run's units net to zero).
	if res.RateSample != nil {
		t.Errorf("known-failed skip recorded a rate sample %+v, want none", res.RateSample)
	}
}

// TestRetranscribeAcceptSkips: an accept-only plan does nothing but records the count.
func TestRetranscribeAcceptSkips(t *testing.T) {
	work := t.TempDir()
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 60, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: 60, Duration: 60}}})
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionAccept, Reason: "false positive"})

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"accepted": 1, "retranscribed": 0})
	if fake.count(2) != 0 {
		t.Errorf("an accept entry transcribed chapter 2 %d times, want 0", fake.count(2))
	}
}

// TestRetranscribeStallMarkerWrittenWhenNoProgress is the convergence-signal regression:
// a repair round that neither splices nor adopts anything (here a retranscribe entry whose
// fresh run is implausible for the chapter duration, so it is KEPT) writes the
// retranscribe_stalled marker qaAdjudicate parks on - the loop achieved nothing.
func TestRetranscribeStallMarkerWrittenWhenNoProgress(t *testing.T) {
	work := t.TempDir()
	// A 60s chapter: the fake backend's 3-word fresh run is implausibly slow -> kept (not
	// adopted), so spliced == 0 && adopted == 0 -> no progress.
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 60, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: 60, Duration: 60}}})
	orig := transcript.Transcript{Schema: transcript.Schema, Chapter: 2, Segments: []transcript.Segment{{ID: 0, Start: 0, End: 60, Text: " ORIGINAL KEEP ME"}}}
	seedNormalized(t, work, orig)
	seedFLACs(t, work, 2)
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionRetranscribe, Reason: "check"})

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"adopted": 0, "kept": 1, "clips_spliced": 0})
	if !fileExistsT(filepath.Join(work, retranscribeStalledMarker)) {
		t.Error("a no-progress round did not write the stall marker")
	}
}

// TestRetranscribeStallMarkerClearedOnProgress: a round that DOES make progress (a splice)
// removes any stale stall marker, so a productive loop never falsely parks the next round.
func TestRetranscribeStallMarkerClearedOnProgress(t *testing.T) {
	work := t.TempDir()
	loop, dur := tailLoopTranscript(2)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: dur, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: dur, Duration: dur}}})
	seedNormalized(t, work, loop)
	seedFLACs(t, work, 2)
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionTailClip, Reason: "tail loop"})
	// A stale marker from a prior no-progress round: this productive round must clear it.
	if err := os.WriteFile(filepath.Join(work, retranscribeStalledMarker), []byte("1\n"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	exe.clipCutter = func(_ context.Context, _, dstFlac string, _, _ float64) error {
		return os.WriteFile(dstFlac, []byte("clip"), 0o644) //nolint:gosec // test artifact
	}
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"clips_spliced": 1})
	if fileExistsT(filepath.Join(work, retranscribeStalledMarker)) {
		t.Error("a progress round (splice) did not clear the stall marker")
	}
}

// TestRetranscribeTailClipResumeIdempotent is the item-2 regression: a 2-entry
// tail_clip plan interrupted after the first chapter resumes without re-cutting,
// re-transcribing, or re-splicing the completed chapter - repairs.log ends with
// exactly one line per chapter and the resumed run never re-transcribes the finished
// chapter's clip.
func TestRetranscribeTailClipResumeIdempotent(t *testing.T) {
	work := t.TempDir()
	loop2, dur := tailLoopTranscript(2)
	loop3, _ := tailLoopTranscript(3)
	writeManifestStruct(t, work, audio.Manifest{
		Source: "/x", Style: audio.StyleMarkers, Duration: dur, ChapterCount: 3,
		Chapters: []audio.Chapter{
			{Chapter: 2, Start: 0, End: dur, Duration: dur},
			{Chapter: 3, Start: 0, End: dur, Duration: dur},
		},
	})
	seedNormalized(t, work, loop2)
	seedNormalized(t, work, loop3)
	seedFLACs(t, work, 3)
	seedPlan(t, work,
		qa.PlanEntry{Chapter: 2, Action: qa.ActionTailClip, Reason: "tail loop"},
		qa.PlanEntry{Chapter: 3, Action: qa.ActionTailClip, Reason: "tail loop"},
	)
	fakeCutter := func(_ context.Context, _, dstFlac string, _, _ float64) error {
		return os.WriteFile(dstFlac, []byte("clip"), 0o644) //nolint:gosec // test artifact
	}

	// Run 1: cancel the context while chapter 2 (the first entry) is being clip-
	// transcribed. The fake backend ignores the cancelled ctx and finishes writing the
	// clip, so chapter 2 fully splices; the loop then returns cleanly at chapter 3's
	// top-of-iteration ctx check, leaving chapter 3 untouched.
	ctx, cancel := context.WithCancel(context.Background())
	b1 := newFakeBackend()
	b1.before = func(ch int) {
		if ch == 2 {
			cancel()
		}
	}
	exe1 := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(b1), Fallback: scheduler.NewStubExecutor(0, 0)})
	exe1.clipCutter = fakeCutter
	if _, err := exe1.Execute(ctx, store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("run 1 error = %v, want context.Canceled (interrupted after ch2)", err)
	}
	if !fileExistsT(filepath.Join(work, transcript.RepairedDir, transcript.TextName(2))) {
		t.Fatal("chapter 2 not spliced in run 1")
	}
	if fileExistsT(filepath.Join(work, transcript.RepairedDir, transcript.TextName(3))) {
		t.Fatal("chapter 3 spliced in run 1, want it deferred to the resume")
	}

	// Run 2 (resume): a fresh counting backend. Chapter 2 is skipped (already repaired),
	// chapter 3 is processed.
	b2 := newFakeBackend()
	exe2 := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(b2), Fallback: scheduler.NewStubExecutor(0, 0)})
	exe2.clipCutter = fakeCutter
	res, err := exe2.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("run 2 (resume): %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"clips_spliced": 2})
	if b2.count(2) != 0 {
		t.Errorf("chapter 2 re-transcribed %d times on resume, want 0 (skipped)", b2.count(2))
	}
	if b2.count(3) != 1 {
		t.Errorf("chapter 3 transcribed %d times on resume, want 1", b2.count(3))
	}
	logRaw, err := os.ReadFile(filepath.Join(work, repair.RepairsLogName))
	if err != nil {
		t.Fatalf("repairs.log missing: %v", err)
	}
	if n := strings.Count(string(logRaw), "ch002"); n != 1 {
		t.Errorf("repairs.log has %d ch002 lines, want exactly 1:\n%s", n, logRaw)
	}
	if n := strings.Count(string(logRaw), "ch003"); n != 1 {
		t.Errorf("repairs.log has %d ch003 lines, want exactly 1:\n%s", n, logRaw)
	}
}

// tailLoopTranscript builds a chapter transcript (>= 50 tokens) whose final ~30 words
// are a repeated 6-word phrase reaching the end - a locatable, adjudicable tail loop.
// It returns the transcript and the chapter duration in seconds.
func tailLoopTranscript(chapter int) (transcript.Transcript, float64) {
	var words []transcript.Word
	var text strings.Builder
	tsec := 0.0
	add := func(w string) {
		words = append(words, transcript.Word{W: " " + w, Start: tsec, End: tsec + 0.4})
		text.WriteString(" " + w)
		tsec += 0.4
	}
	for w := range strings.FieldsSeq("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu apple mango cherry grape") {
		add(w)
	}
	phrase := []string{"wait", "for", "the", "sign", "now", "please"}
	for range 5 {
		for _, w := range phrase {
			add(w)
		}
	}
	seg := transcript.Segment{ID: 0, Start: 0, End: tsec, Text: text.String(), Words: words}
	return transcript.Transcript{Schema: transcript.Schema, Chapter: chapter, Segments: []transcript.Segment{seg}}, tsec
}

// TestRetranscribeLoopsBackToQASweep drives a book queued at retranscribing (with an
// accept-only plan and clean transcripts) through the real scheduler and asserts it
// advances retranscribing -> qa_sweep (whose sentinel advance() clears, so the sweep
// RE-RUNS) -> and, finding the book clean, on through the stub stages to done.
func TestRetranscribeLoopsBackToQASweep(t *testing.T) {
	dir := t.TempDir()
	workRoot := filepath.Join(dir, "work")
	work := filepath.Join(workRoot, "fixture")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	writeQAManifest(t, work, map[int]float64{1: 600, 2: 600, 3: 600})
	for _, ch := range []int{1, 2, 3} {
		writeQATranscript(t, work, ch, cleanSegs())
		// The downstream M5 stages (spelling_research/correcting/validating) read
		// transcripts-text/, which a real sanitize run would have produced - seed it so
		// the book can run all the way to done past the qa_sweep loop-back.
		if err := transcript.WriteText(filepath.Join(work, transcript.TextDir), ch, "hello there reader the story begins now onward we go swiftly"); err != nil {
			t.Fatal(err)
		}
	}
	seedPlan(t, work, qa.PlanEntry{Chapter: 1, Action: qa.ActionAccept, Reason: "benign"})

	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hub := events.NewHub(1024)
	fake := newFakeBackend()
	// A full fake agent drives the M5 agent stages past the qa_sweep re-run to done.
	fakeAgent := newFakeRunner()
	fakeAgent.act = fullFakeAct(t, fullFakeOpts{title: "Book"})
	cfg := fullFakeConfig(dir, fakeAgent)
	cfg.DB, cfg.ASR = db, fakeASR(fake)
	cfg.Fallback = scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond)
	exe := NewExecutor(cfg)
	sched := scheduler.New(db, hub, exe, 2, workRoot, false)

	book, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(dir, "b.m4b"), WorkDir: work, Title: "Book"})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	// Start the book at retranscribing (as if adjudication just queued a repair).
	if err := db.SetBookPipelineState(context.Background(), book.ID, string(state.Retranscribing)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = sched.Start(ctx) }()
	sched.Notify()

	final := waitState(t, db, book.ID, "done", 30*time.Second)
	cancel()
	<-done

	if final.State != "done" {
		t.Fatalf("book state = %q (status %q err %q), want done", final.State, final.Status, final.Error)
	}
	// qa_sweep re-ran after retranscribing (its sentinel + report exist).
	if !scheduler.SentinelExists(work, string(state.QASweep)) {
		t.Error("qa_sweep sentinel missing - the loop-back did not re-run the sweep")
	}
	if _, err := os.Stat(filepath.Join(work, "qa_report.json")); err != nil {
		t.Errorf("qa_report.json missing - qa_sweep did not re-run: %v", err)
	}
}

func assertRetranscribeMetrics(t *testing.T, raw json.RawMessage, want map[string]int) {
	t.Helper()
	var got map[string]int
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse retranscribe metrics: %v (%s)", err, raw)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("metric %q = %d, want %d (full: %v)", k, got[k], v, got)
		}
	}
}
