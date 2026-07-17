package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/asr"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// genNonContiguousM4B builds a tiny 3-chapter .m4b whose chapter markers number 1, 2, 4
// (a gap), so inspect reports markers as NON-contiguous and routes the book through the
// markers_normalizing agent stage. Each chapter is 2s.
func genNonContiguousM4B(t *testing.T, ffmpeg, dir string) string {
	t.Helper()
	titles := []string{"Chapter 1", "Chapter 2", "Chapter 4"} // numbers 1,2,4 -> non-contiguous
	const secs = 2
	var meta strings.Builder
	meta.WriteString(";FFMETADATA1\ntitle=Loopy Book\n")
	for i, title := range titles {
		meta.WriteString("[CHAPTER]\nTIMEBASE=1/1000\n")
		meta.WriteString("START=" + strconv.Itoa(i*secs*1000) + "\n")
		meta.WriteString("END=" + strconv.Itoa((i+1)*secs*1000) + "\n")
		meta.WriteString("title=" + title + "\n")
	}
	metaPath := filepath.Join(dir, "meta.txt")
	if err := os.WriteFile(metaPath, []byte(meta.String()), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	out := filepath.Join(dir, "loopy.m4b")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration="+strconv.Itoa(secs*len(titles)),
		"-i", metaPath, "-map", "0:a", "-map_metadata", "1", "-c:a", "aac", out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("generate non-contiguous m4b: %v", err)
	}
	return out
}

// loopBackend is a scripted asr.Backend for the M5 full-machine test: it emits a
// degenerate (5 identical mid-chapter segments) transcript for chapter 2 on its FIRST
// transcription - so the qa sweep flags it mid-chapter and the adjudicator can queue a
// retranscribe - and a clean single-segment transcript on every other call (every other
// chapter, and chapter 2's re-transcription in the retranscribing stage). It never
// touches a real model. The degenerate run is 20 words over a 2s chapter (600 wpm), so
// the fresh 3-word re-run (90 wpm) is adopted by the plausibility check.
type loopBackend struct {
	mu    sync.Mutex
	calls map[int]int // chapter -> transcribe count
}

func newLoopBackend() *loopBackend { return &loopBackend{calls: map[int]int{}} }

func (b *loopBackend) ID() string { return "loop" }

func (b *loopBackend) Detect(context.Context) (asr.Capability, error) {
	return asr.Capability{Backend: "loop", Available: true, Device: "cpu", Version: "fake"}, nil
}

func (b *loopBackend) EnsureReady(context.Context) error { return nil }

func (b *loopBackend) Transcribe(ctx context.Context, job asr.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	b.calls[job.Chapter]++
	attempt := b.calls[job.Chapter]
	b.mu.Unlock()

	var raw string
	if job.Chapter == 2 && attempt == 1 {
		// A mid-chapter loop: 5 identical segments at the chapter start (< 85% position),
		// each 4 words = 20 words over a 2s chapter (implausibly fast for real narration).
		var segs []string
		for i := range 5 {
			start := float64(i) * 0.4
			segs = append(segs, fmt.Sprintf(`{"id":%d,"start":%.1f,"end":%.1f,"text":" the same phrase here"}`, i, start, start+0.4))
		}
		raw = `{"text":" the same phrase here","language":"en","segments":[` + strings.Join(segs, ",") + `]}`
	} else {
		raw = fmt.Sprintf(`{"text":" fake chapter %d","language":"en","segments":[{"id":0,"start":0,"end":1,"text":" fake chapter %d","avg_logprob":NaN,"words":[{"word":" fake","start":0,"end":0.5,"probability":0.9}]}]}`, job.Chapter, job.Chapter)
	}
	// Both real backends name the raw <audio-stem>.json - hand-mirrored here (not via
	// asr.RawOutputName) so a wrapper or helper that drifts from the tools' naming fails
	// this test.
	stem := strings.TrimSuffix(filepath.Base(job.Audio), filepath.Ext(job.Audio))
	return os.WriteFile(filepath.Join(job.OutDir, stem+".json"), []byte(raw+"\n"), 0o644) //nolint:gosec // test artifact
}

func (b *loopBackend) count(chapter int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[chapter]
}

// TestPipelineFullMachineToDone is the M5 end-to-end drive: a real non-contiguous m4b
// through the real scheduler with a fake ASR backend and the full fake agent, exercising
// every conditional branch of the state machine:
//
//	queued -> inspecting -> markers_normalizing -> splitting -> asr -> sanitizing
//	-> qa_sweep(dirty) -> qa_adjudicating(retranscribe ch2) -> retranscribing
//	-> qa_sweep(clean) -> spelling_research -> correcting -> fact_pass -> synthesizing
//	-> validating -> auditing(fail) -> fixing -> validating -> auditing(pass)
//	-> ready -> contributing -> done
//
// It asserts the cleared-sentinel re-runs (qa_sweep, validating, and auditing each ran
// twice; fixing once) and that the agent stage_runs carry nonzero usage.
func TestPipelineFullMachineToDone(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	src := genNonContiguousM4B(t, ffmpeg, dir)
	workRoot := filepath.Join(dir, "work")

	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hub := events.NewHub(1024)

	backend := newLoopBackend()
	fakeAgent := newFakeRunner()
	fakeAgent.act = fullFakeAct(t, fullFakeOpts{
		title:      "Loopy Book",
		adjudicate: map[int]qa.PlanAction{2: qa.ActionRetranscribe},
		auditFail:  1, // one failing audit round -> a fix loop
	})
	cfg := fullFakeConfig(dir, fakeAgent)
	cfg.DB, cfg.FFmpeg, cfg.FFprobe, cfg.ASR = db, ffmpeg, ffprobe, fakeASR(backend)
	cfg.Fallback = scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond)
	exe := NewExecutor(cfg)
	sched := scheduler.New(db, hub, exe, 2, workRoot, false)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = sched.Start(ctx) }()

	b, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: src,
		WorkDir:    filepath.Join(workRoot, "loopy"),
		Title:      "Loopy Book",
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	sched.Notify()

	final := waitState(t, db, b.ID, "done", 60*time.Second)
	cancel()
	<-done

	if final.State != "done" {
		t.Fatalf("book state = %q (status %q, err %q), want done", final.State, final.Status, final.Error)
	}

	// Every conditional branch was traversed exactly the expected number of times.
	// markers_normalizing/qa_adjudicating/retranscribing each ran once (the branch was
	// taken); qa_sweep/validating/auditing ran twice (a cleared sentinel re-ran the
	// stage); fixing once (the single failing audit round).
	wantRuns := map[state.State]int{
		state.MarkersNormalizing: 1,
		state.QAAdjudicating:     1,
		state.Retranscribing:     1,
		state.QASweep:            2,
		state.Validating:         2,
		state.Auditing:           2,
		state.Fixing:             1,
	}
	for st, want := range wantRuns {
		got, cerr := db.CountStageSuccesses(context.Background(), b.ID, string(st))
		if cerr != nil {
			t.Fatalf("count %s successes: %v", st, cerr)
		}
		if got != want {
			t.Errorf("%s ran %d times, want %d", st, got, want)
		}
	}

	// The retranscribe leg re-ran chapter 2 (asr once + retranscribing once) and left it
	// alone as the loop-back's evidence; other chapters were transcribed once.
	if got := backend.count(2); got != 2 {
		t.Errorf("chapter 2 transcribed %d times, want 2 (asr + retranscribe)", got)
	}

	// The mechanical stages left their real artifacts (not stub sentinels): the corrected
	// manifest, corrections/spellings, the fact knowledge-final sheet, and the sidecars.
	for _, rel := range []string{
		"corrections.json", "spellings.json", "qa_plan.json",
		filepath.Join(factsDir, knowledgeFinalName),
		filepath.Join(sidecarsDir, charactersFileName),
		filepath.Join(sidecarsDir, recapsFileName),
	} {
		if _, serr := os.Stat(filepath.Join(b.WorkDir, rel)); serr != nil {
			t.Errorf("expected artifact %s missing: %v", rel, serr)
		}
	}

	// Agent stage_runs carry the usage the fake reported (cost capture end to end).
	runs, err := db.ListStageRuns(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	usageByStage := map[string]int64{}
	for _, r := range runs {
		usageByStage[r.Stage] += r.InputTokens + r.OutputTokens
	}
	for _, st := range []state.State{state.SpellingResearch, state.FactPass, state.Synthesizing, state.Auditing} {
		if usageByStage[string(st)] <= 0 {
			t.Errorf("agent stage %s recorded zero token usage, want > 0", st)
		}
	}
}
