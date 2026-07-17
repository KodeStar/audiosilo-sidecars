package pipeline

import (
	"context"
	"encoding/json"
	"errors"
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
