package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/repair"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// --- shared seed helpers ---

func writeManifestStruct(t *testing.T, work string, m audio.Manifest) {
	t.Helper()
	if err := audio.WriteManifest(work, m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func seedProbe(t *testing.T, work string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, audio.ProbeName), []byte(`{"chapters":[]}`), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

func seedNormalized(t *testing.T, work string, tr transcript.Transcript) {
	t.Helper()
	if err := transcript.WriteNormalized(filepath.Join(work, transcript.JSONDir), tr); err != nil {
		t.Fatalf("seed normalized ch%d: %v", tr.Chapter, err)
	}
}

func markerChapters(nums ...int) []audio.Chapter {
	chs := make([]audio.Chapter, 0, len(nums))
	for i, n := range nums {
		chs = append(chs, audio.Chapter{Chapter: n, Start: float64(i * 2), End: float64(i*2 + 2), Duration: 2})
	}
	return chs
}

// contiguousDraftManifest is a valid 1,2,3 markers manifest an agent might produce.
func correctedManifest() audio.Manifest {
	return audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 3)}
}

// --- markers_normalizing ---

func TestMarkersNormalizeHappyPath(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	// A non-contiguous draft (1,2,4) - the reason the book reached this stage.
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, audio.ManifestName, correctedManifest())
		writeOut(t, req, "verdict.json", markerVerdict{Confident: true, Reason: "excluded opening credits"})
		return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 120, Output: 60, CostUSD: 0.02, Turns: 2}}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("markers_normalize: %v", err)
	}
	// The corrected, contiguous manifest replaced the draft.
	m, err := audio.ReadManifest(work)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !audio.Contiguous(m.Chapters) || len(m.Chapters) != 3 {
		t.Errorf("manifest not the corrected contiguous map: %+v", m.Chapters)
	}
	if !scheduler.SentinelExists(work, string(state.MarkersNormalizing)) {
		t.Error("markers sentinel missing")
	}
	assertUsageMetrics(t, res.Metrics, "sonnet", 120, 60)
	// The agent stage requested the routed model.
	if r, ok := fake.lastRequest(string(state.MarkersNormalizing)); !ok || r.Model != "sonnet" || r.Web {
		t.Errorf("agent request model=%q web=%v, want sonnet/false", r.Model, r.Web)
	}
}

// TestAgentStageRateSampleExcludesBackoff drives markers_normalizing through a
// rate-limit backoff (first attempt rate-limited, second succeeds) with a short
// injected backoff, and asserts the reported RateSample charges only productive agent
// time: 1 unit, and Seconds well below the backoff the run actually slept through.
func TestAgentStageRateSampleExcludesBackoff(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	const backoff = 300 * time.Millisecond
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		if attempt == 1 {
			// Rate-limit the first attempt: RunWithBackoff sleeps `backoff`, then retries.
			return agent.Result{}, &agent.RateLimitError{Detail: "429"}
		}
		writeOut(t, req, audio.ManifestName, correctedManifest())
		writeOut(t, req, "verdict.json", markerVerdict{Confident: true, Reason: "ok"})
		return agent.Result{Usage: agent.Usage{Model: "sonnet"}}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	exe.backoff = []time.Duration{backoff} // tiny schedule so the test does not sleep for minutes
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("markers_normalize: %v", err)
	}
	if fake.count(string(state.MarkersNormalizing)) != 2 {
		t.Fatalf("agent ran %d times, want 2 (one rate-limited + one success)", fake.count(string(state.MarkersNormalizing)))
	}
	if res.RateSample == nil {
		t.Fatal("no RateSample; want one")
	}
	if res.RateSample.Units != 1 {
		t.Errorf("RateSample.Units = %d, want 1 (one whole-book agent stage)", res.RateSample.Units)
	}
	// The stage's wall-clock spanned the ~300ms backoff, but the rate charges only
	// productive agent time, so Seconds must be well under the backoff it slept through.
	if res.RateSample.Seconds >= backoff.Seconds() {
		t.Errorf("RateSample.Seconds = %v, want < %v (rate-limit backoff excluded)", res.RateSample.Seconds, backoff.Seconds())
	}
}

func TestMarkersNormalizeNotConfidentParks(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	draft := audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)}
	writeManifestStruct(t, work, draft)

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, audio.ManifestName, correctedManifest())
		writeOut(t, req, "verdict.json", markerVerdict{Confident: false, Reason: "one marker holds two chapters"})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError", err)
	}
	if !strings.HasPrefix(pe.Reason, MarkersNotConfidentPrefix) || !strings.Contains(pe.Reason, "one marker holds two chapters") {
		t.Errorf("park reason = %q, want the %q prefix + the verdict reason", pe.Reason, MarkersNotConfidentPrefix)
	}
	// The draft was NOT overwritten (still non-contiguous) and no sentinel written.
	m, _ := audio.ReadManifest(work)
	if audio.Contiguous(m.Chapters) {
		t.Error("draft manifest was overwritten on a not-confident verdict")
	}
	if scheduler.SentinelExists(work, string(state.MarkersNormalizing)) {
		t.Error("sentinel written despite parking")
	}
}

// TestMarkersNormalizeNotConfidentNoManifestParksOnce is the item-3 regression: an
// agent that follows the "do not guess" instruction (a not-confident verdict and NO
// out/manifest.json) parks the book needs_attention with its own reason in ONE
// invocation - not after exhausting the retry budget with the wrong message.
func TestMarkersNormalizeNotConfidentNoManifestParksOnce(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		// ONLY a not-confident verdict, no manifest - the validator must accept this.
		writeOut(t, req, "verdict.json", markerVerdict{Confident: false, Reason: "markers are retail samples"})
		return agent.Result{Usage: agent.Usage{Model: "sonnet"}}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError", err)
	}
	if !strings.Contains(pe.Reason, "markers are retail samples") {
		t.Errorf("park reason = %q, want the agent's verdict reason", pe.Reason)
	}
	if n := fake.count(string(state.MarkersNormalizing)); n != 1 {
		t.Errorf("agent invoked %d times, want 1 (a not-confident verdict is valid, no retries)", n)
	}
	if scheduler.SentinelExists(work, string(state.MarkersNormalizing)) {
		t.Error("sentinel written despite parking")
	}
}

func TestMarkersNormalizeInvalidManifestExhaustsAndParks(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		// Always produce a NON-contiguous manifest (1,2,4) - the validator rejects it.
		writeOut(t, req, audio.ManifestName, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})
		writeOut(t, req, "verdict.json", markerVerdict{Confident: true, Reason: "done"})
		return agent.Result{Usage: agent.Usage{Model: "sonnet"}}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError after validation exhaustion", err)
	}
	if !strings.HasPrefix(pe.Reason, AgentValidationExhaustedPrefix) {
		t.Errorf("park reason = %q, want the %q prefix", pe.Reason, AgentValidationExhaustedPrefix)
	}
	// 3 attempts total (2 retries), and the runner saw the appended validator error.
	if n := fake.count(string(state.MarkersNormalizing)); n != 3 {
		t.Errorf("agent invoked %d times, want 3 (initial + 2 retries)", n)
	}
	if !strings.Contains(fake.lastPrompt(string(state.MarkersNormalizing)), "contiguous") {
		t.Errorf("retry prompt did not carry the validator error; got %q", fake.lastPrompt(string(state.MarkersNormalizing)))
	}
}

func TestMarkersNormalizeAgentUnavailableParks(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	exe := NewExecutor(Config{DataDir: t.TempDir(), Fallback: scheduler.NewStubExecutor(0, 0)})
	// No agent, and re-detection finds none (this machine may have a real claude CLI,
	// which is not what this test is about).
	exe.redetectAgent = func(context.Context) (agent.Runner, agent.Availability) {
		return nil, agent.Availability{Detail: "no agent CLI found"}
	}
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError", err)
	}
	if pe.Reason != AgentUnavailableMsg {
		t.Errorf("park reason = %q, want AgentUnavailableMsg", pe.Reason)
	}
}

// --- qa_adjudicating ---

// seedQAReport writes qa_report.json/.md flagging the given retranscribe-queue chapters
// plus a manifest so the adjudicating stage has both artifacts.
func seedQAReport(t *testing.T, work string, queue []int) *qa.Report {
	t.Helper()
	rep := &qa.Report{Chapters: 3, RetranscribeQueue: queue}
	if err := qa.WriteReport(work, rep); err != nil {
		t.Fatalf("write qa report: %v", err)
	}
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 30, ChapterCount: 3, Chapters: markerChapters(1, 2, 3)})
	return rep
}

func TestQAAdjudicateAcceptAll(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "harmless closing echo"}}})
		return agent.Result{Usage: agent.Usage{Model: "sonnet"}}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	if res.RetranscribeNeeded {
		t.Error("RetranscribeNeeded = true for an accept-all plan, want false")
	}
	if _, err := os.Stat(filepath.Join(work, qa.PlanFile)); err != nil {
		t.Errorf("qa_plan.json not harvested: %v", err)
	}
	if !scheduler.SentinelExists(work, string(state.QAAdjudicating)) {
		t.Error("qa_adjudicating sentinel missing")
	}
}

func TestQAAdjudicateRetranscribePlan(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionRetranscribe, Reason: "mid-chapter loss"}}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	if !res.RetranscribeNeeded {
		t.Error("RetranscribeNeeded = false for a retranscribe plan, want true")
	}
}

func TestQAAdjudicateInvalidPlanRetries(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		// Plan omits the flagged chapter 2 -> plan.Validate fails every round.
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError after validation exhaustion", err)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 3 {
		t.Errorf("agent invoked %d times, want 3", n)
	}
	if !strings.Contains(fake.lastPrompt(string(state.QAAdjudicating)), "flagged for disposition") {
		t.Errorf("retry prompt did not carry the validator error; got %q", fake.lastPrompt(string(state.QAAdjudicating)))
	}
}

func TestQAAdjudicateRoundCapParks(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	book, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(dir, "b.m4b"), WorkDir: work, Title: "Book"})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	// Three prior successful adjudication rounds -> the cap trips.
	for i := range 3 {
		runID, serr := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), i+1)
		if serr != nil {
			t.Fatal(serr)
		}
		if ferr := db.FinishStageRun(context.Background(), runID, true, nil); ferr != nil {
			t.Fatal(ferr)
		}
	}
	fake := newFakeRunner()
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.DB = db
	exe := NewExecutor(cfg)
	_, err = exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError (round cap)", err)
	}
	if pe.Reason != QANoConvergeMsg {
		t.Errorf("park reason = %q, want %q", pe.Reason, QANoConvergeMsg)
	}
	if fake.count(string(state.QAAdjudicating)) != 0 {
		t.Error("the agent was invoked despite the round cap")
	}
}

// TestQAAdjudicateAutoAcceptsRepairedTails is the item-4 regression: a report whose
// only flagged chapter is tail-flagged AND already repaired via tail_clip is
// auto-accepted by the pipeline with NO agent invocation, yielding an accept-all plan
// and RetranscribeNeeded=false so the book advances to spelling_research rather than
// looping to the round cap on the agent's goodwill.
func TestQAAdjudicateAutoAcceptsRepairedTails(t *testing.T) {
	work := t.TempDir()
	// A tail-rate-only report flagging chapter 2 (its only finding is tail-related).
	rep := &qa.Report{Chapters: 3, TailRate: []qa.TailRateHit{{Chapter: 2, WPS: 5, Span: 2, Tail: "do do do"}}}
	if err := qa.WriteReport(work, rep); err != nil {
		t.Fatalf("write report: %v", err)
	}
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 30, ChapterCount: 3, Chapters: markerChapters(1, 2, 3)})
	// The durable evidence of a completed tail_clip: a repaired splice + a verdict entry.
	if err := transcript.WriteText(filepath.Join(work, transcript.RepairedDir), 2, "the real ending text"); err != nil {
		t.Fatal(err)
	}
	if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: 2, Verdict: repair.VerdictBenign}); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		t.Errorf("agent invoked for an all-auto-accept round (stage %q)", req.Stage)
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 0 {
		t.Errorf("agent invoked %d times, want 0 (all chapters auto-accepted)", n)
	}
	if res.RetranscribeNeeded {
		t.Error("RetranscribeNeeded = true, want false (accept-all)")
	}
	plan, err := qa.LoadPlan(work)
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Chapter != 2 || plan.Entries[0].Action != qa.ActionAccept {
		t.Errorf("plan = %+v, want a single accept entry for chapter 2", plan.Entries)
	}
	next, _, err := state.NextState(state.QAAdjudicating, state.Outcome{RetranscribeNeeded: res.RetranscribeNeeded})
	if err != nil {
		t.Fatal(err)
	}
	if next != state.SpellingResearch {
		t.Errorf("next state = %q, want spelling_research", next)
	}
}

// f64ptr returns a pointer to v, for the optional *float64 report fields.
func f64ptr(v float64) *float64 { return &v }

// newTailClipAdjudicateFixture seeds a work dir whose QA report flags a single tail_rate
// chapter (5) alongside a 6-chapter manifest, and builds an executor whose fake agent
// always dispositions chapter 5 as one tail_clip. It is the shared setup for the two
// fixed-point tests, which diverge only in what they do between adjudication rounds.
func newTailClipAdjudicateFixture(t *testing.T) (work string, exe *Executor, fake *fakeRunner, book store.Book) {
	t.Helper()
	work = t.TempDir()
	rep := &qa.Report{Chapters: 6, TailRate: []qa.TailRateHit{{Chapter: 5, WPS: 9, Tail: "loop"}}}
	if err := qa.WriteReport(work, rep); err != nil {
		t.Fatal(err)
	}
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 30, ChapterCount: 6, Chapters: markerChapters(1, 2, 3, 4, 5, 6)})
	fake = newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 5, Action: qa.ActionTailClip, Reason: "tail loop"}}})
		return agent.Result{}, nil
	}
	exe = NewExecutor(withAgentConfig(t.TempDir(), fake))
	book = store.Book{ID: 1, Title: "Book", WorkDir: work}
	return work, exe, fake, book
}

// TestQAAdjudicateFixedPointParks is the item-3 regression: once a round adjudicates a
// report and the next sweep is bit-identical with an unchanged verdict ledger (the
// repairs moved nothing), the stage parks WITHOUT another agent round, deletes the
// fingerprint, and a user Retry gets exactly one fresh round before the fixed point
// re-parks (never the 3-round burn). The fixture's agent dispositions chapter 5 as a
// tail_clip, but nothing here rewrites qa_report.json or tail_verdicts.json (the latter
// never exists), so the next round's fingerprint inputs are identical.
func TestQAAdjudicateFixedPointParks(t *testing.T) {
	work, exe, fake, book := newTailClipAdjudicateFixture(t)
	stage := string(state.QAAdjudicating)
	fpPath := filepath.Join(work, "qa_round_fingerprint")

	// Round 1: no prior fingerprint -> the agent runs; a plan + fingerprint are written.
	if _, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if n := fake.count(stage); n != 1 {
		t.Fatalf("round 1 agent calls = %d, want 1", n)
	}
	if _, err := os.Stat(fpPath); err != nil {
		t.Fatalf("fingerprint not written after round 1: %v", err)
	}

	// Round 2: report unchanged and a plan exists -> fixed point: park, no agent, and the
	// fingerprint is deleted.
	_, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("round 2 error = %v, want a ParkError (fixed point)", err)
	}
	if pe.Code != state.ParkQANoConverge {
		t.Errorf("park code = %q, want %q", pe.Code, state.ParkQANoConverge)
	}
	if !strings.Contains(pe.Reason, "fixed point") || !strings.Contains(pe.Reason, "5") {
		t.Errorf("park reason = %q, want a fixed-point message naming chapter 5", pe.Reason)
	}
	if n := fake.count(stage); n != 1 {
		t.Errorf("round 2 agent calls = %d, want still 1 (no agent on a fixed point)", n)
	}
	if _, serr := os.Stat(fpPath); !os.IsNotExist(serr) {
		t.Errorf("fingerprint must be deleted on a fixed-point park, stat err = %v", serr)
	}

	// A user Retry: the deleted fingerprint means exactly ONE fresh agent round runs.
	if _, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("retry round: %v", err)
	}
	if n := fake.count(stage); n != 2 {
		t.Errorf("retry agent calls = %d, want 2 (one fresh round after Retry)", n)
	}
	// The very next round re-parks (fixed point again) without a third agent call.
	_, err = exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{})
	if !errors.As(err, &pe) {
		t.Fatalf("post-retry error = %v, want a ParkError (fixed point re-parks)", err)
	}
	if n := fake.count(stage); n != 2 {
		t.Errorf("post-retry agent calls = %d, want still 2 (re-park without agent)", n)
	}
}

// TestQAAdjudicateSpliceProgressNotFixedPoint is the false-park regression on the
// fingerprint design: the tail_rate/cross_segment detectors read the raw
// transcripts-json layer (golden contract), which a SPLICE does not touch - so a round
// whose tail_clips all succeeded leaves qa_report.json bit-identical while
// tail_verdicts.json gained the splice's verdict (real progress). Because the
// fingerprint covers report+verdicts, the next round must run normally (auto-accept /
// agent), NOT park as a fixed point.
func TestQAAdjudicateSpliceProgressNotFixedPoint(t *testing.T) {
	work, exe, fake, book := newTailClipAdjudicateFixture(t)

	// Round 1: agent queues a tail_clip; the fingerprint records report + empty ledger.
	if _, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("round 1: %v", err)
	}

	// Between rounds: the retranscribing stage SUCCESSFULLY splices chapter 5 - the
	// repaired file appears and tail_verdicts.json gains a BENIGN entry, but the re-sweep
	// report (reading the untouched transcripts-json layer) is bit-identical.
	if err := transcript.WriteText(filepath.Join(work, transcript.RepairedDir), 5, "the real ending"); err != nil {
		t.Fatal(err)
	}
	if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: 5, ClipStart: 20, Verdict: repair.VerdictBenign}); err != nil {
		t.Fatal(err)
	}

	// Round 2: NOT a fixed point (the ledger moved). The round runs normally - here the
	// repaired chapter auto-accepts (tail-only + splice evidence), no agent, no park.
	res, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("round 2 must not park as a fixed point after a successful splice: %v", err)
	}
	if res.RetranscribeNeeded {
		t.Error("round 2 RetranscribeNeeded = true, want false (the repaired chapter auto-accepts)")
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 1 {
		t.Errorf("round 2 agent calls = %d, want still 1 (auto-accept, no agent)", n)
	}
	plan, err := qa.LoadPlan(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Chapter != 5 || plan.Entries[0].Action != qa.ActionAccept {
		t.Errorf("round 2 plan = %+v, want a single auto-accept for chapter 5", plan.Entries)
	}
}

// dbBackedQAExecutor opens a real store, creates a book at work dir `work`, and returns a
// db-backed executor with the fake agent - the setup the fingerprint-lifecycle tests need
// so CountStageSuccesses is live (withAgentConfig alone leaves e.db nil).
func dbBackedQAExecutor(t *testing.T, work string, fake *fakeRunner) (*store.DB, *Executor, store.Book) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	book, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(t.TempDir(), "b.m4b"), WorkDir: work, Title: "Book"})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.DB = db
	return db, NewExecutor(cfg), book
}

// TestQAAdjudicateRoundCapClearsFingerprint is the item-4(a) regression: the 3-round cap
// park deletes qa_round_fingerprint too, so a user Retry after a round-cap park (like after
// a fixed-point park) gets a clean slate rather than immediately re-parking on a stale
// fixed-point signal.
func TestQAAdjudicateRoundCapClearsFingerprint(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})
	fake := newFakeRunner() // never invoked - the round cap parks before any agent round
	db, exe, book := dbBackedQAExecutor(t, work, fake)
	// 3 completed rounds -> the round cap fires.
	for i := 0; i < 3; i++ {
		runID, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), i+1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishStageRun(context.Background(), runID, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	fpPath := filepath.Join(work, "qa_round_fingerprint")
	if err := os.WriteFile(fpPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) || pe.Code != state.ParkQANoConverge {
		t.Fatalf("error = %v, want a ParkQANoConverge park (round cap)", err)
	}
	if strings.Contains(pe.Reason, "fixed point") {
		t.Errorf("round-cap park reason = %q, want the 3-round message not the fixed-point one", pe.Reason)
	}
	if _, serr := os.Stat(fpPath); !os.IsNotExist(serr) {
		t.Errorf("round-cap park must delete the fingerprint, stat err = %v", serr)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 0 {
		t.Errorf("agent called %d times on a round-cap park, want 0", n)
	}
}

// TestQAAdjudicateStaleFingerprintRunsAgent is the item-5 regression: when the round budget
// is reset (CountStageSuccesses == 0) but a MATCHING fingerprint + plan are still on disk
// (stale leftovers a Retry/purge-rewind/crash left behind), the stale-fingerprint guard
// drops them so the round runs a fresh agent pass instead of falsely parking as a fixed
// point - the documented contract that a reset round budget always gets one fresh round.
func TestQAAdjudicateStaleFingerprintRunsAgent(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "harmless echo"}}})
		return agent.Result{}, nil
	}
	db, exe, book := dbBackedQAExecutor(t, work, fake)
	// Open the stage run (agent usage target) but record NO successes -> done == 0.
	if _, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), 1); err != nil {
		t.Fatal(err)
	}
	// Seed a plan + a MATCHING fingerprint: without the stale guard this reads as a fixed
	// point (fingerprint == current report+ledger state AND a plan exists).
	if err := qa.WritePlan(work, &qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionTailClip, Reason: "tail loop"}}}); err != nil {
		t.Fatal(err)
	}
	fp, err := qaRoundFingerprint(work)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeQAFingerprint(work, fp); err != nil {
		t.Fatal(err)
	}

	if _, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("done==0 with a stale fingerprint must run the agent, not park: %v", err)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 1 {
		t.Errorf("agent called %d times, want 1 (a reset round budget gets one fresh round)", n)
	}
}

// TestTailOnlyChaptersTailResiduals drives the widened tail-residual classification
// (item-1) over incident-shaped fixtures: a cross-segment / non-mid multi-loop finding
// whose time or position sits in the chapter's spliced tail zone no longer disqualifies
// the chapter, while a mid-chapter finding, a wph outlier, a within-segment hit, a
// straddling span (starts mid-chapter, ends in the tail), or a finding with no covering
// splice still does. It reads the report + a verdict map only.
func TestTailOnlyChaptersTailResiduals(t *testing.T) {
	verdicts := map[int]repair.TailVerdict{
		2:  {ClipStart: 425.7},
		8:  {ClipStart: 826.1},
		10: {ClipStart: 977.8},
		11: {ClipStart: 500.0},
		12: {ClipStart: 300.0},
		13: {ClipStart: 400.0},
		20: {ClipStart: 100.0},
		21: {ClipStart: 100.0},
		30: {ClipStart: 826.1},
	}
	rep := &qa.Report{
		Chapters: 30,
		// Every listed chapter is flagged via a tail_rate hit (so it is required).
		TailRate: []qa.TailRateHit{
			{Chapter: 2}, {Chapter: 8}, {Chapter: 10}, {Chapter: 11}, {Chapter: 12},
			{Chapter: 13}, {Chapter: 20}, {Chapter: 21}, {Chapter: 25}, {Chapter: 30},
		},
		CrossSegment: []qa.CrossSegmentHit{
			// ch2: located span starts inside the tail (430 >= 425.7-15) -> covered.
			{Chapter: 2, Count: 6, FirstSec: f64ptr(430), LastSec: f64ptr(450), Pos: 99},
			// ch8: the real incident case - span 814-845s, clip_start 826.1: FirstSec
			// 814 >= 811.1 -> covered (the whole span begins in the tail zone).
			{Chapter: 8, Count: 6, FirstSec: f64ptr(814), LastSec: f64ptr(845), Pos: 98},
			// ch11: a genuine mid-chapter cross hit (starts 100s, clip_start 500) -> NOT covered.
			{Chapter: 11, Count: 6, FirstSec: f64ptr(100), LastSec: f64ptr(120), Pos: 20},
			// ch12: no usable time, position in the tail (>= 95) -> covered.
			{Chapter: 12, Count: 6, Pos: 97},
			// ch13: no usable time, "-1.0% (?)" not-located -> NOT covered.
			{Chapter: 13, Count: 6, Pos: -1},
			// ch30: a STRADDLING span - starts mid-chapter (790s) but ends in the tail
			// (845s) past clip_start 826.1. Testing FirstSec (790 < 811.1) -> NOT covered,
			// so a hit that ate real narration before the loop is not auto-accepted.
			{Chapter: 30, Count: 6, FirstSec: f64ptr(790), LastSec: f64ptr(845), Pos: 96},
		},
		MultiLoop: []qa.MultiLoopFinding{
			// ch10: a non-mid multi-loop located in the tail -> covered.
			{Chapter: 10, Count: 6, AtSec: f64ptr(985), Pos: 96, MidChapter: false},
			// ch20: a MID-CHAPTER multi-loop -> always disqualifies.
			{Chapter: 20, Count: 6, AtSec: f64ptr(200), Pos: 40, MidChapter: true},
		},
		WithinSegment: []qa.WithinSegmentHit{
			// ch21: a within-segment loop always disqualifies (even in the tail).
			{Chapter: 21, Count: 8, Pos: 99},
		},
		WPHOutliers: []qa.WPHOutlier{
			{Chapter: 25, WPH: 9000, Z: 4}, // ch25: wph outlier always disqualifies.
		},
		RetranscribeQueue: []int{25},
	}
	got := tailOnlyChapters(rep, verdicts)
	wantTailOnly := map[int]bool{2: true, 8: true, 10: true, 12: true}
	notTailOnly := []int{11, 13, 20, 21, 25, 30}
	for ch := range wantTailOnly {
		if !got[ch] {
			t.Errorf("chapter %d should be tail-only (a covered tail residual)", ch)
		}
	}
	for _, ch := range notTailOnly {
		if got[ch] {
			t.Errorf("chapter %d should NOT be tail-only", ch)
		}
	}
}

// TestAutoAcceptRepairedTailsIncident reproduces the production report shape: 8 chapters
// with a successful splice and only tail-zone residual findings auto-accept, while two
// CLIP-REDEGENERATED chapters (verdict only, no repaired file) and a wph-outlier +
// mid-chapter chapter do not.
func TestAutoAcceptRepairedTailsIncident(t *testing.T) {
	work := t.TempDir()
	spliced := map[int]float64{2: 425.7, 8: 826.1, 10: 977.8, 14: 1217.7, 15: 937.8, 21: 1746.4, 22: 1086.3, 24: 1263.7}
	redegen := []int{5, 16} // CLIP-REDEGENERATED: verdict only, no repaired file
	var tailFlagged []qa.TailRateHit
	var crossHits []qa.CrossSegmentHit
	for ch, cs := range spliced {
		tailFlagged = append(tailFlagged, qa.TailRateHit{Chapter: ch})
		// A cross-segment residual sitting in the tail (last segment past clip_start).
		crossHits = append(crossHits, qa.CrossSegmentHit{Chapter: ch, Count: 6, FirstSec: f64ptr(cs - 5), LastSec: f64ptr(cs + 10), Pos: 98})
		// Durable evidence of a completed splice: repaired file + a verdict entry.
		if err := transcript.WriteText(filepath.Join(work, transcript.RepairedDir), ch, "the real ending"); err != nil {
			t.Fatal(err)
		}
		if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: ch, ClipStart: cs, Verdict: repair.VerdictBenign}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ch := range redegen {
		tailFlagged = append(tailFlagged, qa.TailRateHit{Chapter: ch})
		// A CLIP-REDEGENERATED verdict (no repaired file) - has a clip_start, but not "done".
		if err := repair.MergeTailVerdict(work, repair.TailVerdict{Chapter: ch, ClipStart: 500, Verdict: repair.VerdictClipRedegenerated}); err != nil {
			t.Fatal(err)
		}
	}
	// ch25: a wph outlier + a mid-chapter run - never tail-only, never repaired.
	rep := &qa.Report{
		Chapters:          30,
		TailRate:          tailFlagged,
		CrossSegment:      crossHits,
		WPHOutliers:       []qa.WPHOutlier{{Chapter: 25, WPH: 9000, Z: 4}},
		RepeatedRuns:      []qa.RepeatedRun{{Chapter: 25, Kind: qa.KindMidChapter, Length: 5}},
		RetranscribeQueue: []int{25},
	}

	entries := (&Executor{}).autoAcceptRepairedTails(rep, work)
	got := map[int]bool{}
	for _, en := range entries {
		if en.Action != qa.ActionAccept {
			t.Errorf("chapter %d auto-entry action = %q, want accept", en.Chapter, en.Action)
		}
		got[en.Chapter] = true
	}
	for ch := range spliced {
		if !got[ch] {
			t.Errorf("chapter %d (spliced, tail-residual only) should auto-accept", ch)
		}
	}
	for _, ch := range append(redegen, 25) {
		if got[ch] {
			t.Errorf("chapter %d should NOT auto-accept", ch)
		}
	}
	if len(entries) != len(spliced) {
		t.Errorf("auto-accepted %d chapters, want %d", len(entries), len(spliced))
	}
}

func TestQAAdjudicateRecordsUsage(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	seedQAReport(t, work, []int{2})
	book, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(dir, "b.m4b"), WorkDir: work, Title: "Book"})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	// Open the stage run the scheduler would open, so AddOpenStageRunUsage has a target.
	if _, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), 1); err != nil {
		t.Fatal(err)
	}
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "benign"}}})
		return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 100, Output: 50, CostUSD: 0.02}}, nil
	}
	cfg := withAgentConfig(t.TempDir(), fake)
	cfg.DB = db
	exe := NewExecutor(cfg)
	if _, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	runs, err := db.ListStageRuns(context.Background(), book.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range runs {
		if r.Stage == string(state.QAAdjudicating) {
			found = true
			if r.Model != "sonnet" || r.InputTokens != 100 || r.OutputTokens != 50 {
				t.Errorf("stage run usage = model %q in %d out %d, want sonnet/100/50", r.Model, r.InputTokens, r.OutputTokens)
			}
		}
	}
	if !found {
		t.Error("no qa_adjudicating stage run recorded")
	}
}

// --- invariant: staged dirs hold exactly the contracted inputs ---

func TestAgentStagedDirsHoldOnlyContractedInputs(t *testing.T) {
	work := t.TempDir()
	seedProbe(t, work)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x/book.m4b", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})

	markersFake := newFakeRunner()
	markersFake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, audio.ManifestName, correctedManifest())
		writeOut(t, req, "verdict.json", markerVerdict{Confident: true, Reason: "ok"})
		return agent.Result{}, nil
	}
	mexe := NewExecutor(withAgentConfig(t.TempDir(), markersFake))
	if _, err := mexe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, scheduler.StageReport{}); err != nil {
		t.Fatalf("markers: %v", err)
	}
	mReq, _ := markersFake.lastRequest(string(state.MarkersNormalizing))
	// The markers staged dir must contain NO transcript files (it is pre-transcription).
	walkAssertNo(t, mReq.Dir, "transcripts")

	// Adjudicating: only the FLAGGED chapter's transcript is staged.
	work2 := t.TempDir()
	seedQAReport(t, work2, []int{2})
	for _, ch := range []int{1, 2, 3} {
		seedText(t, work2, ch)
	}
	adjFake := newFakeRunner()
	adjFake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "ok"}}})
		return agent.Result{}, nil
	}
	aexe := NewExecutor(withAgentConfig(t.TempDir(), adjFake))
	if _, err := aexe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work2}, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("adjudicating: %v", err)
	}
	aReq, _ := adjFake.lastRequest(string(state.QAAdjudicating))
	staged := filepath.Join(aReq.Dir, transcript.TextDir)
	if !fileExistsT(filepath.Join(staged, transcript.TextName(2))) {
		t.Error("flagged chapter 2 transcript was not staged")
	}
	for _, ch := range []int{1, 3} {
		if fileExistsT(filepath.Join(staged, transcript.TextName(ch))) {
			t.Errorf("unflagged chapter %d transcript was staged (spoiler-scope leak)", ch)
		}
	}
}

func seedText(t *testing.T, work string, chapter int) {
	t.Helper()
	if err := transcript.WriteText(filepath.Join(work, transcript.TextDir), chapter, "chapter text"); err != nil {
		t.Fatal(err)
	}
}

func walkAssertNo(t *testing.T, root, substr string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.Contains(rel, substr) {
			t.Errorf("staged dir contains a forbidden %q file: %s", substr, rel)
		}
		return nil
	})
}

func fileExistsT(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// assertUsageMetrics unmarshals a stage's usage metrics and checks the headline fields.
func assertUsageMetrics(t *testing.T, raw json.RawMessage, model string, in, out int64) {
	t.Helper()
	var m struct {
		Usage struct {
			Model        string `json:"model"`
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
			Invocations  int    `json:"invocations"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse usage metrics: %v (%s)", err, raw)
	}
	if m.Usage.Model != model || m.Usage.InputTokens != in || m.Usage.OutputTokens != out {
		t.Errorf("usage metrics = %+v, want model %s in %d out %d", m.Usage, model, in, out)
	}
	if m.Usage.Invocations < 1 {
		t.Errorf("usage invocations = %d, want >= 1", m.Usage.Invocations)
	}
}
