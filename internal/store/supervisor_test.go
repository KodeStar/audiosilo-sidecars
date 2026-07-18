package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestSupervisorPersistenceAndBatchCostAggregation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batch, err := db.CreateBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBook(ctx, NewBook{BatchID: batch.ID, SourcePath: "/book", WorkDir: t.TempDir(), Title: "Book"})
	if err != nil {
		t.Fatal(err)
	}
	run1, _ := db.StartStageRun(ctx, b.ID, "fact_pass", 1)
	est := 0.22
	if err := db.AddOpenStageRunUsageDetailed(ctx, b.ID, "fact_pass", "codex-x", 100, 50, 25, 0, false, est, true); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(ctx, run1, false, json.RawMessage(`{"error":"failed"}`)); err != nil {
		t.Fatal(err)
	}
	run2, _ := db.StartStageRun(ctx, b.ID, "fact_pass", 2)
	if err := db.AddOpenStageRunUsageDetailed(ctx, b.ID, "fact_pass", "opus", 100, 50, 10, 1.25, true, 0.3, true); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(ctx, run2, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SupersedeStageSuccesses(ctx, b.ID, "fact_pass"); err != nil {
		t.Fatal(err)
	}
	budgetCost, budgetComplete, err := db.SumStageRunBudgetCost(ctx, b.ID)
	if err != nil || !budgetComplete || math.Abs(budgetCost-1.47) > 1e-9 {
		t.Fatalf("effective budget cost=%v complete=%v err=%v", budgetCost, budgetComplete, err)
	}
	bid := b.ID
	provider := 0.10
	estimated := 0.12
	sid, err := db.StartSupervisorRun(ctx, SupervisorRun{BatchID: batch.ID, BookID: &bid, Trigger: "test", Diagnosis: "x", Evidence: json.RawMessage(`[]`), SelectedAction: "observe", State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSupervisorRun(ctx, SupervisorRun{ID: sid, Diagnosis: "x", Confidence: 1, Evidence: json.RawMessage(`[]`), Decision: "test", SelectedAction: "observe", State: "completed", ProviderCostUSD: &provider, EstimatedAPICostUSD: &estimated}); err != nil {
		t.Fatal(err)
	}
	batchEst := 0.05
	_, err = db.StartSupervisorRun(ctx, SupervisorRun{BatchID: batch.ID, Trigger: "batch", Diagnosis: "queue", Evidence: json.RawMessage(`[]`), SelectedAction: "observe", State: "completed", EstimatedAPICostUSD: &batchEst})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartSupervisorRun(ctx, SupervisorRun{BatchID: batch.ID, Trigger: "unknown", Diagnosis: "unknown-cost call", Evidence: json.RawMessage(`[]`), SelectedAction: "observe", State: "failed", Model: "unpriced", Backend: "codex", ModelCalls: 2}); err != nil {
		t.Fatal(err)
	}
	if calls, err := db.SupervisorInvocationCountSince(ctx, timestamp(time.Now().Add(-time.Hour))); err != nil || calls != 2 {
		t.Fatalf("model calls=%d err=%v", calls, err)
	}
	costs, err := db.BatchCosts(ctx, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if costs.ProductionReportedUSD != 1.25 || costs.ProductionEstimatedAPIUSD != 0.52 {
		t.Fatalf("production costs=%+v", costs)
	}
	if costs.BookSupervisorReportedUSD != 0.10 || costs.BookSupervisorEstimatedAPIUSD != 0.12 || costs.BatchSupervisorEstimatedAPIUSD != 0.05 {
		t.Fatalf("supervisor costs=%+v", costs)
	}
	if math.Abs(costs.OverallReportedUSD-1.35) > 1e-9 || math.Abs(costs.OverallEstimatedAPIUSD-0.69) > 1e-9 {
		t.Fatalf("overall costs=%+v", costs)
	}
	if !costs.ProductionReportedIncomplete || costs.ProductionEstimateIncomplete || !costs.SupervisorReportedIncomplete || !costs.SupervisorEstimateIncomplete || !costs.OverallReportedIncomplete || !costs.OverallEstimateIncomplete {
		t.Fatalf("unknown-cost flags=%+v", costs)
	}
	// Failed and superseded calls remain in both ledgers.
	runs, err := db.ListStageRuns(ctx, b.ID)
	if err != nil || len(runs) != 2 || !runs[1].Superseded {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}

func TestMigrationAddsSupervisorDefaultsToLegacyDatabase(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "0009_" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply legacy %s: %v", name, err)
		}
		if _, err := legacy.ExecContext(ctx, `INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?)`, name, "2026-01-01T00:00:00.000000000Z"); err != nil {
			t.Fatal(err)
		}
	}
	created := "2026-01-01T00:00:00.000000000Z"
	res, err := legacy.ExecContext(ctx, `INSERT INTO books(source_path, work_dir, title, created_at, updated_at) VALUES('/legacy','/work','Legacy',?,?)`, created, created)
	if err != nil {
		t.Fatal(err)
	}
	bookID, _ := res.LastInsertId()
	if _, err := legacy.ExecContext(ctx, `INSERT INTO stage_runs(book_id, stage, attempt, started_at, finished_at, ok, metrics, model, input_tokens, output_tokens, cost_usd) VALUES(?, 'fact_pass', 1, ?, ?, 1, '{}', 'legacy-model', 10, 2, 0.25)`, bookID, created, created); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b, err := db.GetBook(ctx, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if b.BatchID != LegacyBatchID {
		t.Fatalf("migrated batch=%q", b.BatchID)
	}
	runs, err := db.ListStageRuns(ctx, bookID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("migrated runs=%+v err=%v", runs, err)
	}
	if !runs[0].CostReported || runs[0].EstimatedAPICostUSD != nil || runs[0].EstimateComplete || runs[0].HeartbeatAt != created || runs[0].ProgressAt != created {
		t.Fatalf("migrated cost/liveness defaults=%+v", runs[0])
	}
	if err := db.EnsureBatch(ctx, "simulated", time.Now()); err != nil {
		t.Fatal(err)
	}
}
