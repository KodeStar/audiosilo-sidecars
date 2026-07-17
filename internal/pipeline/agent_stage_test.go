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
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, nil)
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
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, nil)
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
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, nil)
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
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, nil)
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
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, nil)
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
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, nil)
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
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, nil)
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
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, nil)
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
	_, err = exe.Execute(context.Background(), book, state.QAAdjudicating, nil)
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
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, nil)
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
	if _, err := exe.Execute(context.Background(), book, state.QAAdjudicating, nil); err != nil {
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
	if _, err := mexe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.MarkersNormalizing, nil); err != nil {
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
	if _, err := aexe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work2}, state.QAAdjudicating, nil); err != nil {
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
