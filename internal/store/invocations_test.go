package store

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func TestConcurrentAgentInvocationAccountingAndLifecycle(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, err := db.CreateBook(ctx, NewBook{SourcePath: "/inv", WorkDir: "/work/inv", Title: "Invocations"})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := db.StartStageRun(ctx, b.ID, "fact_pass", 1)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 3)
	for i := range ids {
		ids[i], err = db.StartAgentInvocation(ctx, b.ID, "fact_pass", string(rune('a'+i)), "claude", "sonnet")
		if err != nil {
			t.Fatal(err)
		}
		if err = db.SetAgentInvocationProcess(ctx, ids[i], 100+i, true); err != nil {
			t.Fatal(err)
		}
	}
	total, byBook, err := db.ActiveAgentInvocationCounts(ctx)
	if err != nil || total != 3 || byBook[b.ID] != 3 {
		t.Fatalf("active=%d byBook=%v err=%v", total, byBook, err)
	}
	statuses := []string{"success", "failure", "cancelled"}
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := db.FinishAgentInvocation(ctx, id, statuses[i], "sonnet", int64(i+1), int64(i+2), int64(i), float64(i+1), true, float64(i+1)/2, true, ""); e != nil {
				t.Errorf("finish: %v", e)
			}
		}()
	}
	wg.Wait()
	runs, err := db.ListStageRuns(ctx, b.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	r := runs[0]
	if r.InputTokens != 6 || r.OutputTokens != 9 || r.CacheReadTokens != 3 || r.CostUSD != 6 || r.ProcessActive {
		t.Fatalf("aggregate=%+v", r)
	}
	if err := db.FinishStageRun(ctx, runID, true, json.RawMessage(`{"completed":true}`)); err != nil {
		t.Fatal(err)
	}
	inv, err := db.ListAgentInvocations(ctx, b.ID)
	if err != nil || len(inv) != 3 {
		t.Fatalf("invocations=%v err=%v", inv, err)
	}
	for i, v := range inv {
		if v.Status != statuses[i] || v.Active {
			t.Errorf("invocation %d=%+v", i, v)
		}
	}
	if err := db.SupersedeStageSuccesses(ctx, b.ID, "fact_pass"); err != nil {
		t.Fatal(err)
	}
	runs, err = db.ListStageRuns(ctx, b.ID)
	if err != nil || !runs[0].Superseded {
		t.Fatalf("superseded run=%+v err=%v", runs, err)
	}
	if cost, err := db.SumStageRunCost(ctx, b.ID); err != nil || cost != 6 {
		t.Fatalf("cost after lifecycle=%v,%v", cost, err)
	}
}

func TestBookTimingUsesStablePrimaryASRCompletion(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, err := db.CreateBook(ctx, NewBook{SourcePath: "/timing", WorkDir: "/work/timing", Title: "Timing"})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(stage, start, finish string, ok int, superseded int) {
		t.Helper()
		_, err := db.sql.ExecContext(ctx, `INSERT INTO stage_runs(book_id,stage,attempt,started_at,finished_at,ok,metrics,superseded) VALUES(?,?,?,?,?,?, '{}',?)`, b.ID, stage, 1, start, finish, ok, superseded)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("inspecting", "2026-01-01T00:00:00.000000000Z", "2026-01-01T00:00:10.000000000Z", 1, 0)
	insert("splitting", "2026-01-01T00:00:20.000000000Z", "2026-01-01T00:00:30.000000000Z", 1, 0)
	insert("asr", "2026-01-01T00:00:40.000000000Z", "2026-01-01T00:00:50.000000000Z", 1, 1)
	insert("asr", "2026-01-01T00:01:00.000000000Z", "2026-01-01T00:01:30.000000000Z", 1, 0)
	insert("sanitizing", "2026-01-01T00:01:40.000000000Z", "2026-01-01T00:02:00.000000000Z", 1, 0)
	if err := db.SetBookPipelineState(ctx, b.ID, "done"); err != nil {
		t.Fatal(err)
	}
	got, err := db.BookTiming(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrimaryASRCompletedAt != "2026-01-01T00:01:30.000000000Z" || got.PreASRWallSeconds != 90 || got.ASRActiveSeconds != 40 || got.PostASRElapsedSeconds != 30 || got.ActiveProcessingSeconds != 80 || got.QueueWaitSeconds != 40 || got.BatchElapsedSeconds != 120 {
		t.Fatalf("timing=%+v", got)
	}
}
