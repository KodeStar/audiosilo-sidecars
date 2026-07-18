package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// distinctChapterTranscript builds a chapter of >= 50 DISTINCT words (no repeated 6-gram), so
// LocateTailRun returns ok=false - the shape a SHORT tail repeat (below the 6-gram locator's
// reach) leaves behind. It returns the transcript and its duration in seconds.
func distinctChapterTranscript(chapter int) (transcript.Transcript, float64) {
	var words []transcript.Word
	var text strings.Builder
	tsec := 0.0
	for w := range strings.FieldsSeq("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu apple mango cherry grape lemon melon orange peach plum berry olive maple cedar birch aspen willow poplar spruce fir larch yew hazel rowan elm ivy sage thyme basil") {
		words = append(words, transcript.Word{W: " " + w, Start: tsec, End: tsec + 0.4})
		text.WriteString(" " + w)
		tsec += 0.4
	}
	seg := transcript.Segment{ID: 0, Start: 0, End: tsec, Text: text.String(), Words: words}
	return transcript.Transcript{Schema: transcript.Schema, Chapter: chapter, Segments: []transcript.Segment{seg}}, tsec
}

// TestRetranscribeTailClipUnlocatableBuckets is the item-1/2 metrics regression: a tail_clip
// entry with NO clip_start_sec whose chapter has no locatable loop is a NO-OP that the stage
// buckets as clips_unlocatable (NOT clips_redegenerated - it did no ASR at all), emits a stage
// Note naming the chapter and asking for a clip_start_sec, runs no ASR, writes no repaired
// file, and records no rate sample (a no-work round nets zero productive units).
func TestRetranscribeTailClipUnlocatableBuckets(t *testing.T) {
	work := t.TempDir()
	tr, dur := distinctChapterTranscript(2)
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: dur, ChapterCount: 1, Chapters: []audio.Chapter{{Chapter: 2, Start: 0, End: dur, Duration: dur}}})
	seedNormalized(t, work, tr)
	seedFLACs(t, work, 2)
	// A tail_clip with NO clip_start_sec: the mechanical 6-gram locator finds no loop.
	seedPlan(t, work, qa.PlanEntry{Chapter: 2, Action: qa.ActionTailClip, Reason: "short tail repeat"})

	var notes []string
	rep := scheduler.StageReport{Note: func(m string) { notes = append(notes, m) }}
	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	// A cutter is resolved because the plan has a tail_clip, but the locator no-ops before any cut.
	exe.clipCutter = func(_ context.Context, _, dstFlac string, _, _ float64) error {
		return os.WriteFile(dstFlac, []byte("clip"), 0o644) //nolint:gosec // test artifact
	}
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Retranscribing, rep)
	if err != nil {
		t.Fatalf("retranscribing: %v", err)
	}
	assertRetranscribeMetrics(t, res.Metrics, map[string]int{"clips_unlocatable": 1, "clips_redegenerated": 0, "clips_spliced": 0})
	if fake.count(2) != 0 {
		t.Errorf("an unlocatable no-op ran ASR %d times, want 0", fake.count(2))
	}
	if fileExistsT(filepath.Join(work, transcript.RepairedDir, transcript.TextName(2))) {
		t.Error("an unlocatable no-op must not write a repaired file")
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "could not locate a loop in chapter 2") && strings.Contains(n, "clip_start_sec") {
			found = true
		}
	}
	if !found {
		t.Errorf("missing the unlocatable stage note; got %v", notes)
	}
	// A no-ASR round records no rate sample (done - completed - unlocatableNew == 0 units).
	if res.RateSample != nil {
		t.Errorf("an unlocatable-only round recorded a rate sample %+v, want none", res.RateSample)
	}
}

// TestQAAdjudicateLedgerSkipsAgentWhenAllCovered is the item-3 core: a chapter the agent
// accepted in round 1 is recorded in the durable qa_accepted.json ledger, so round 2 (whose
// re-sweep re-flags the same chapter off the stale layer) accepts it mechanically and does NOT
// invoke the agent again - the round-cap burner is closed.
func TestQAAdjudicateLedgerSkipsAgentWhenAllCovered(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2})
	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 2, Action: qa.ActionAccept, Reason: "harmless closing echo"}}})
		return agent.Result{Usage: agent.Usage{Model: "sonnet"}}, nil
	}
	db, exe, book := dbBackedQAExecutor(t, work, fake)

	// Round 1: the agent accepts chapter 2. Open+finish the run so CountStageSuccesses == 1.
	runID, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if err := db.FinishStageRun(context.Background(), runID, true, nil); err != nil {
		t.Fatal(err)
	}
	if fake.count(string(state.QAAdjudicating)) != 1 {
		t.Fatalf("round 1 agent calls = %d, want 1", fake.count(string(state.QAAdjudicating)))
	}
	// The ledger recorded chapter 2 as an agent accept in round 1.
	led := loadAcceptedLedger(work)
	if e2, ok := led[2]; !ok || e2.Source != "agent" || e2.Round != 1 {
		t.Fatalf("ledger[2] = %+v (ok=%v), want an agent accept from round 1", led[2], ok)
	}

	// Round 2: the same report still flags chapter 2, but the ledger covers it -> no agent.
	if _, err := db.StartStageRun(context.Background(), book.ID, string(state.QAAdjudicating), 2); err != nil {
		t.Fatal(err)
	}
	res, err := exe.Execute(context.Background(), book, state.QAAdjudicating, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 1 {
		t.Errorf("agent invoked %d times total, want 1 (round 2 must be all-mechanical)", n)
	}
	if res.RetranscribeNeeded {
		t.Error("round 2 RetranscribeNeeded = true, want false (all accepted)")
	}
	plan, err := qa.LoadPlan(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Chapter != 2 || plan.Entries[0].Action != qa.ActionAccept {
		t.Errorf("round 2 plan = %+v, want a single mechanical accept for chapter 2", plan.Entries)
	}
	if !strings.Contains(plan.Entries[0].Reason, "accepted round 1") {
		t.Errorf("round 2 accept reason = %q, want it to cite the original round", plan.Entries[0].Reason)
	}
}

// TestQAAdjudicateLedgerExcludesAcceptedFromAgentSet: a chapter already in the ledger is folded
// into the mechanical-accept set, so when the report ALSO flags a fresh chapter the agent is
// asked to disposition only the fresh one - a plan that omits the ledger chapter still validates
// (the merge covers it), and the persisted plan carries both.
func TestQAAdjudicateLedgerExcludesAcceptedFromAgentSet(t *testing.T) {
	work := t.TempDir()
	seedQAReport(t, work, []int{2, 3})
	// Pre-seed the ledger: chapter 2 was accepted in a prior round.
	if err := writeAcceptedLedger(work, map[int]acceptedEntry{2: {Round: 1, Reason: "benign echo", Source: "agent"}}); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	fake.act = func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		// The agent dispositions ONLY chapter 3; if the ledger fold failed, plan.Validate would
		// reject this for omitting the still-flagged chapter 2.
		writeOut(t, req, qa.PlanFile, qa.Plan{Entries: []qa.PlanEntry{{Chapter: 3, Action: qa.ActionAccept, Reason: "false positive"}}})
		return agent.Result{}, nil
	}
	exe := NewExecutor(withAgentConfig(t.TempDir(), fake))
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, Title: "Book", WorkDir: work}, state.QAAdjudicating, scheduler.StageReport{}); err != nil {
		t.Fatalf("qa_adjudicating: %v", err)
	}
	if n := fake.count(string(state.QAAdjudicating)); n != 1 {
		t.Errorf("agent invoked %d times, want 1 (the fresh chapter only, no validation retries)", n)
	}
	// The agent's prompt named chapter 2 as already-accepted (do not disposition).
	if p := fake.lastPrompt(string(state.QAAdjudicating)); !strings.Contains(p, "2") {
		t.Errorf("prompt did not list the ledger chapter as already-accepted:\n%s", p)
	}
	plan, err := qa.LoadPlan(work)
	if err != nil {
		t.Fatal(err)
	}
	byCh := map[int]qa.PlanEntry{}
	for _, en := range plan.Entries {
		byCh[en.Chapter] = en
	}
	if e2, ok := byCh[2]; !ok || e2.Action != qa.ActionAccept || !strings.Contains(e2.Reason, "accepted round 1") {
		t.Errorf("merged plan chapter 2 = %+v (ok=%v), want a mechanical ledger accept", byCh[2], ok)
	}
	if e3, ok := byCh[3]; !ok || e3.Action != qa.ActionAccept {
		t.Errorf("merged plan chapter 3 = %+v (ok=%v), want the agent's accept", byCh[3], ok)
	}
	// The round persisted chapter 3's fresh accept into the ledger too.
	led := loadAcceptedLedger(work)
	if e3, ok := led[3]; !ok || e3.Source != "agent" {
		t.Errorf("ledger[3] = %+v (ok=%v), want a newly-recorded agent accept", led[3], ok)
	}
}
