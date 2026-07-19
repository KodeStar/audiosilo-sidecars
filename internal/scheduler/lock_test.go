package scheduler

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

func bk(id int64, series, pos, st string) store.Book {
	return store.Book{ID: id, Series: series, SeriesPos: pos, State: st}
}

func TestLockHoldersSeriesOrdering(t *testing.T) {
	// Same series, positions 1 and 2, both mid-pipeline: only pos 1 holds.
	books := []store.Book{
		bk(1, "S", "1", string(state.FactPass)),
		bk(2, "S", "2", string(state.ASR)),
	}
	h := lockHolders(books)
	if !h[1] || h[2] {
		t.Fatalf("holders = %v, want only book 1", h)
	}

	// Pos 1 reaches ready: pos 2 becomes the holder.
	books[0].State = string(state.Ready)
	h = lockHolders(books)
	if h[1] || !h[2] {
		t.Fatalf("after pos1 ready, holders = %v, want only book 2", h)
	}
}

func TestSortASRLaneBreadthFirstAcrossSeries(t *testing.T) {
	// Series A book 1 has already left ASR. Its presence in the full library must
	// keep A2 at depth 1, allowing the first books from B and C to transcribe before
	// ASR loops back to A. The input ids reflect depth-first enqueue order.
	all := []store.Book{
		bk(1, "A", "1", string(state.SpellingResearch)),
		bk(2, "A", "2", string(state.ASR)),
		bk(3, "A", "3", string(state.ASR)),
		bk(4, "B", "1", string(state.ASR)),
		bk(5, "B", "2", string(state.ASR)),
		bk(6, "C", "1", string(state.ASR)),
		bk(7, "", "", string(state.ASR)),
	}
	candidates := []store.Book{all[4], all[2], all[1], all[6], all[5], all[3]}

	sortASRLane(candidates, all)

	want := []int64{4, 6, 7, 2, 5, 3}
	for i, id := range want {
		if candidates[i].ID != id {
			t.Fatalf("ASR order = %v, want %v", bookIDs(candidates), want)
		}
	}
}

func TestSortASRLaneRetranscriptionJumpsBreadthQueue(t *testing.T) {
	all := []store.Book{
		bk(1, "A", "1", string(state.ASR)),
		bk(2, "A", "2", string(state.ASR)),
		bk(3, "B", "1", string(state.ASR)),
		bk(9, "A", "3", string(state.Retranscribing)),
	}
	candidates := append([]store.Book(nil), all...)

	sortASRLane(candidates, all)

	want := []int64{9, 1, 3, 2}
	for i, id := range want {
		if candidates[i].ID != id {
			t.Fatalf("ASR order = %v, want %v", bookIDs(candidates), want)
		}
	}
}

func TestQueueSnapshotSeparatesLiveProcessingFromActualASROrder(t *testing.T) {
	books := []store.Book{
		bk(1, "A", "1", string(state.FactPass)),
		bk(2, "A", "2", string(state.ASR)),
		bk(3, "A", "3", string(state.ASR)),
		bk(4, "B", "1", string(state.ASR)),
		bk(5, "B", "2", string(state.ASR)),
		bk(6, "", "", string(state.Splitting)),
		bk(7, "C", "1", string(state.Synthesizing)),
		bk(8, "C", "2", string(state.SpellingResearch)),
		bk(9, "D", "1", string(state.Retranscribing)),
		{ID: 10, State: string(state.Auditing), Status: string(state.StatusPaused)},
		{ID: 11, State: string(state.Done)},
		bk(12, "E", "1", string(state.SpellingResearch)),
		{ID: 13, State: string(state.FactPass), Status: string(state.StatusPaused)},
	}
	active := map[int64]inflightBook{
		1:  {lane: state.LaneAgent, stage: state.FactPass},
		4:  {lane: state.LaneASR, stage: state.ASR},
		6:  {lane: state.LaneMechanical, stage: state.Splitting},
		7:  {lane: state.LaneAgent, stage: state.Synthesizing},
		9:  {lane: state.LaneASR, stage: state.Retranscribing},
		13: {lane: state.LaneAgent, stage: state.FactPass},
	}

	snapshot := queueSnapshot(books, active)
	assertQueue := func(id int64, group, bucket string, position int, running bool) {
		t.Helper()
		got, ok := snapshot[id]
		if !ok || got.Group != group || got.Bucket != bucket || got.Position != position || got.Active != running {
			t.Errorf("book %d queue = %+v (present %v), want group=%s bucket=%s position=%d active=%v", id, got, ok, group, bucket, position, running)
		}
	}
	// Processing: each independent worker/queue gets its own exact FIFO positions;
	// the lower-id C2 is explicitly blocked because C1 holds its series lock.
	assertQueue(1, queueGroupProcessing, queueBucketAgentActive, 1, true)
	assertQueue(7, queueGroupProcessing, queueBucketAgentActive, 2, true)
	assertQueue(13, queueGroupProcessing, queueBucketAgentActive, 3, true)
	assertQueue(12, queueGroupProcessing, queueBucketAgent, 1, false)
	assertQueue(8, queueGroupProcessing, queueBucketBlocked, 1, false)
	// Corrective and full-book ASR have independent active/ready buckets. The
	// in-flight split remains visible as active preparation, not transcription.
	assertQueue(9, queueGroupASR, queueBucketRetranscribing, 1, true)
	assertQueue(4, queueGroupASR, queueBucketTranscribing, 1, true)
	assertQueue(2, queueGroupASR, queueBucketTranscription, 1, false)
	assertQueue(5, queueGroupASR, queueBucketTranscription, 2, false)
	assertQueue(3, queueGroupASR, queueBucketTranscription, 3, false)
	assertQueue(6, queueGroupASR, queueBucketPreparingMechActive, 1, true)
	if _, ok := snapshot[10]; ok {
		t.Error("idle paused book should not have a live queue placement")
	}
	if _, ok := snapshot[11]; ok {
		t.Error("done book should not have a live queue placement")
	}
}

func TestPartitionASRKeepsRetranscriptionIndependentFromFullBookASR(t *testing.T) {
	candidates := []store.Book{
		bk(9, "A", "3", string(state.Retranscribing)),
		bk(1, "A", "1", string(state.ASR)),
		bk(8, "B", "2", string(state.Retranscribing)),
		bk(2, "B", "1", string(state.ASR)),
	}

	repairs, fullBooks := partitionASR(candidates)
	if got, want := bookIDs(repairs), []int64{9, 8}; !slices.Equal(got, want) {
		t.Fatalf("repairs = %v, want %v", got, want)
	}
	if got, want := bookIDs(fullBooks), []int64{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("full books = %v, want %v", got, want)
	}
}

type blockingASRExecutor struct {
	started chan state.State
}

func (e *blockingASRExecutor) Execute(ctx context.Context, _ store.Book, stage state.State, _ StageReport) (StageResult, error) {
	e.started <- stage
	<-ctx.Done()
	return StageResult{}, ctx.Err()
}

func TestCorrectiveRetranscriptionRunsBesideSerialFullBookASR(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	full := h.addBook(t, db, "full", "A", "1")
	repairOne := h.addBook(t, db, "repair-one", "A", "2")
	repairTwo := h.addBook(t, db, "repair-two", "B", "1")
	ctx := context.Background()
	if err := db.SetBookState(ctx, full.ID, string(state.ASR), "", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{repairOne.ID, repairTwo.ID} {
		if err := db.SetBookState(ctx, id, string(state.Retranscribing), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	exec := &blockingASRExecutor{started: make(chan state.State, 3)}
	sched := New(db, h.hub, exec, 1, h.workRoot, false)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = sched.Start(runCtx); close(done) }()

	seen := map[state.State]int{}
	for range 2 {
		select {
		case stage := <-exec.started:
			seen[stage]++
		case <-time.After(2 * time.Second):
			cancel()
			<-done
			t.Fatal("timed out waiting for full ASR and corrective retranscription to overlap")
		}
	}
	if seen[state.ASR] != 1 || seen[state.Retranscribing] != 1 {
		cancel()
		<-done
		t.Fatalf("concurrent starts = %v, want one full ASR and one retranscription", seen)
	}
	select {
	case stage := <-exec.started:
		cancel()
		<-done
		t.Fatalf("unexpected third ASR worker started for %s", stage)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	<-done
}

func bookIDs(books []store.Book) []int64 {
	ids := make([]int64, len(books))
	for i, b := range books {
		ids[i] = b.ID
	}
	return ids
}

func TestSeriesBlockersReportsOnlyAgentBooksActuallyWaiting(t *testing.T) {
	books := []store.Book{
		{ID: 1, Title: "Saga One", Series: "Saga", SeriesPos: "1", State: string(state.FactPass)},
		{ID: 2, Title: "Saga Two", Series: "Saga", SeriesPos: "2", State: string(state.SpellingResearch)},
		// ASR is allowed to run ahead and therefore must not be labelled series-blocked.
		{ID: 3, Title: "Saga Three", Series: "Saga", SeriesPos: "3", State: string(state.ASR)},
		// A paused agent-stage book is not waiting until it is resumed.
		{ID: 4, Title: "Saga Four", Series: "Saga", SeriesPos: "4", State: string(state.SpellingResearch), Status: string(state.StatusPaused)},
		{ID: 5, Title: "Other One", Series: "Other", SeriesPos: "1", State: string(state.SpellingResearch)},
	}

	blocked := SeriesBlockers(books)
	if len(blocked) != 1 {
		t.Fatalf("blockers = %+v, want only Saga Two", blocked)
	}
	if got := blocked[2]; got.BookID != 1 || got.Title != "Saga One" || got.SeriesPos != "1" {
		t.Fatalf("Saga Two blocker = %+v", got)
	}

	// Once the predecessor reaches Ready it releases the lock and Saga Two becomes
	// the owner, so it must disappear from the waiting map immediately.
	books[0].State = string(state.Ready)
	if got := SeriesBlockers(books); len(got) != 0 {
		t.Fatalf("blockers after predecessor ready = %+v, want none", got)
	}
}

func TestQueueStatsPublishesAndClearsSeriesBlockers(t *testing.T) {
	hub := events.NewHub(8)
	_, sub := hub.Subscribe(0)
	defer sub.Close()
	s := &Scheduler{hub: hub, agentCap: 2}
	books := []store.Book{
		{ID: 1, Title: "Saga One", Series: "Saga", SeriesPos: "1", State: string(state.FactPass)},
		{ID: 2, Title: "Saga Two", Series: "Saga", SeriesPos: "2", State: string(state.SpellingResearch)},
	}
	type statsPayload struct {
		SeriesBlockedBy map[string]SeriesBlocker `json:"series_blocked_by"`
		QueueBooks      []QueueBook              `json:"queue_books"`
	}
	decode := func() statsPayload {
		t.Helper()
		ev := <-sub.C
		if ev.Type != "queue.stats" {
			t.Fatalf("event type = %q", ev.Type)
		}
		var payload statsPayload
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}

	s.publishQueueStats(books, map[state.Lane]int{})
	first := decode()
	if got := first.SeriesBlockedBy["2"]; got.BookID != 1 || got.Title != "Saga One" {
		t.Fatalf("published blocker = %+v", got)
	}
	if len(first.QueueBooks) != 2 || first.QueueBooks[0].Group != queueGroupProcessing || first.QueueBooks[0].Bucket != queueBucketAgent || first.QueueBooks[0].Position != 1 {
		t.Fatalf("published queue books = %+v, want exact Processing bucket placement", first.QueueBooks)
	}

	books[0].State = string(state.Ready)
	s.publishQueueStats(books, map[state.Lane]int{})
	if got := decode().SeriesBlockedBy; len(got) != 0 {
		t.Fatalf("published blockers after release = %+v, want empty", got)
	}
}

func TestLockHoldersParkedPredecessorHoldsSeries(t *testing.T) {
	// Pos 1 is parked (needs_attention) mid-pipeline; it is still unfinished, so
	// it retains the series lock and pos 2 stays blocked from agent work until
	// pos 1 is resumed or cancelled.
	books := []store.Book{
		bk(1, "S", "1", string(state.Auditing)),
		bk(2, "S", "2", string(state.ASR)),
	}
	books[0].Status = string(state.StatusNeedsAttention)
	h := lockHolders(books)
	if !h[1] || h[2] {
		t.Fatalf("parked predecessor: holders = %v, want only book 1", h)
	}
}

func TestParkedPredecessorDoesNotBlockIndependentAgentStages(t *testing.T) {
	books := []store.Book{
		{ID: 1, Series: "S", SeriesPos: "1", State: string(state.Auditing), Status: string(state.StatusNeedsAttention)},
		{ID: 2, Series: "S", SeriesPos: "2", State: string(state.MarkersNormalizing)},
		{ID: 3, Series: "S", SeriesPos: "3", State: string(state.QAAdjudicating)},
		{ID: 4, Series: "S", SeriesPos: "4", State: string(state.SpellingResearch)},
	}
	holders := lockHolders(books)
	if !state.CanStart(state.MarkersNormalizing, state.StatusNone, holders[2]) {
		t.Fatal("marker normalization should continue around a parked predecessor")
	}
	if !state.CanStart(state.QAAdjudicating, state.StatusNone, holders[3]) {
		t.Fatal("QA adjudication should continue around a parked predecessor")
	}
	if state.CanStart(state.SpellingResearch, state.StatusNone, holders[4]) {
		t.Fatal("series-aware authoring must still wait for the parked predecessor")
	}
}

func TestLockHoldersSerieslessAndCrossSeriesParallelize(t *testing.T) {
	books := []store.Book{
		bk(1, "", "", string(state.FactPass)),
		bk(2, "", "", string(state.Synthesizing)),
		bk(3, "A", "1", string(state.FactPass)),
		bk(4, "B", "1", string(state.FactPass)),
	}
	h := lockHolders(books)
	for _, id := range []int64{1, 2, 3, 4} {
		if !h[id] {
			t.Errorf("book %d should hold (seriesless or distinct series): %v", id, h)
		}
	}
}

func TestParkedSeriesDoesNotDeadlockOtherSeries(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	// Series X pos 1 will park at the audit (fix budget spent); pos 2 waits on it.
	h.addBook(t, db, "x1", "X", "1")
	h.addBook(t, db, "x2", "X", "2")
	// An independent seriesless book must still finish.
	free := h.addBook(t, db, "free", "", "")

	exec := NewStubExecutor(2*time.Millisecond, 5*time.Millisecond)
	// x1 always fails the audit -> parks needs_attention after the fix cap.
	exec.Decide = func(b store.Book, stage state.State) StageResult {
		r := StageResult{MarkersContiguous: true, QAClean: true}
		if b.Title == "x1" && stage == state.Auditing {
			r.AuditPassed = false
		} else {
			r.AuditPassed = true
		}
		return r
	}

	pred := func(books []store.Book) bool {
		var x1Parked, freeDone bool
		for _, b := range books {
			if b.ID == free.ID && b.State == string(state.Done) {
				freeDone = true
			}
			if b.Title == "x1" && b.Status == string(state.StatusNeedsAttention) {
				x1Parked = true
			}
		}
		return x1Parked && freeDone
	}
	books := runUntil(t, db, h.hub, exec, 2, pred, 20*time.Second)

	var x1, x2, freeb store.Book
	for _, b := range books {
		switch b.Title {
		case "x1":
			x1 = b
		case "x2":
			x2 = b
		case "free":
			freeb = b
		}
	}
	if x1.Status != string(state.StatusNeedsAttention) {
		t.Fatalf("x1 status = %q, want needs_attention", x1.Status)
	}
	if freeb.State != string(state.Done) {
		t.Fatalf("independent book blocked: state=%s", freeb.State)
	}
	// x2 is held before agent work by the parked predecessor: it may have done
	// mechanical/asr work but must NOT have passed its (agent) fact_pass.
	if state.Order(state.State(x2.State)) > state.Order(state.FactPass) {
		t.Fatalf("x2 advanced past agent gate despite parked predecessor: %s", x2.State)
	}
}

func TestReconcileClosesOpenRunsAndRewindsOnMissingSentinel(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "recon", "", "")

	// Simulate a crash: an open (never-finished) run, a completed 'inspecting'
	// with its sentinel, and a completed 'asr' whose sentinel is MISSING.
	_ = db.SetBookState(ctx, b.ID, string(state.Sanitizing), "", "", "")
	openID, _ := db.StartStageRun(ctx, b.ID, string(state.Sanitizing), 1) // left open
	insID, _ := db.StartStageRun(ctx, b.ID, string(state.Inspecting), 1)
	_ = db.FinishStageRun(ctx, insID, true, nil)
	_ = WriteSentinel(b.WorkDir, string(state.Inspecting), StageResult{})
	asrID, _ := db.StartStageRun(ctx, b.ID, string(state.ASR), 1)
	_ = db.FinishStageRun(ctx, asrID, true, nil)
	// NOTE: no ASR sentinel written -> the reconcile must rewind to asr.

	sched := New(db, h.hub, NewStubExecutor(0, 0), 2, h.workRoot, false)
	sched.ctx = ctx
	if err := sched.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The open run is now closed failed.
	if open, _ := db.OpenStageRuns(ctx); len(open) != 0 {
		t.Fatalf("open runs after reconcile = %d, want 0", len(open))
	}
	runs, _ := db.ListStageRuns(ctx, b.ID)
	var interrupted *store.StageRun
	for i := range runs {
		if runs[i].ID == openID {
			interrupted = &runs[i]
		}
	}
	if interrupted == nil || interrupted.FinishedAt == "" || interrupted.Ok == nil || *interrupted.Ok {
		t.Fatalf("interrupted run not closed failed: %+v", interrupted)
	}
	// The book rewound to asr (earliest completed stage missing its sentinel).
	cur, _ := db.GetBook(ctx, b.ID)
	if cur.State != string(state.ASR) {
		t.Fatalf("rewound to %q, want asr", cur.State)
	}
	// asr's success was dropped; inspecting's kept.
	allSucc, _ := db.SucceededStagesAll(ctx)
	succ := allSucc[b.ID]
	if succ[string(state.ASR)] {
		t.Error("asr success should have been dropped")
	}
	if !succ[string(state.Inspecting)] {
		t.Error("inspecting success should be preserved")
	}
}

func TestControlOpErrors(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "ctrl", "", "")
	sched := New(db, h.hub, NewStubExecutor(0, 0), 2, h.workRoot, false)

	// Resume a non-paused book -> invalid.
	if err := sched.Resume(ctx, b.ID); err != ErrInvalidOp {
		t.Errorf("resume non-paused = %v, want ErrInvalidOp", err)
	}
	// Retry a non-failed book -> invalid.
	if err := sched.Retry(ctx, b.ID); err != ErrInvalidOp {
		t.Errorf("retry non-failed = %v, want ErrInvalidOp", err)
	}
	// Missing book.
	if err := sched.Pause(ctx, 9999); err != store.ErrNotFound {
		t.Errorf("pause missing = %v, want ErrNotFound", err)
	}
	// Cancel then retry works.
	if err := sched.Cancel(ctx, b.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	cur, _ := db.GetBook(ctx, b.ID)
	if cur.Status != string(state.StatusFailed) {
		t.Fatalf("cancelled status = %q", cur.Status)
	}
	if err := sched.Retry(ctx, b.ID); err != nil {
		t.Fatalf("retry after cancel: %v", err)
	}
	// Delete works when not in-flight.
	if err := sched.Delete(ctx, b.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
