package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/pricing"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/supervisor"
)

func TestSupervisorStatusIncidentsCostsAndManualGate(t *testing.T) {
	var service *supervisor.Service
	env := newPipelineEnv(t, nil, func(deps *Deps) {
		service = supervisor.New(deps.Store, deps.Config.Supervisor, pricing.Table{Version: "test-v1"}, nil, supervisor.Hooks{})
		deps.Supervisor = service
	})
	token := env.login(t)
	ctx := context.Background()
	if err := env.db.EnsureBatch(ctx, "api-batch", time.Now()); err != nil {
		t.Fatal(err)
	}
	book, err := env.db.CreateBook(ctx, store.NewBook{BatchID: "api-batch", SourcePath: "/api-book", WorkDir: t.TempDir(), Title: "API Book"})
	if err != nil {
		t.Fatal(err)
	}
	bookID := book.ID
	if _, err := env.db.StartSupervisorRun(ctx, store.SupervisorRun{
		BatchID: "api-batch", BookID: &bookID, Trigger: "test", Diagnosis: "stale heartbeat",
		Evidence: json.RawMessage(`["heartbeat age exceeded"]`), Decision: "stale_heartbeat",
		SelectedAction: "terminate_requeue", State: "decided",
	}); err != nil {
		t.Fatal(err)
	}

	resp := env.do(t, http.MethodGet, "/api/v1/supervisor/status", token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status endpoint = %d", resp.StatusCode)
	}
	var status supervisor.Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.AutomaticActions || status.ModelAssisted || status.ModelAvailable {
		t.Fatalf("conservative status defaults = %+v", status)
	}

	resp = env.do(t, http.MethodGet, "/api/v1/supervisor/incidents?batch_id=api-batch&limit=8", token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("incidents endpoint = %d", resp.StatusCode)
	}
	var incidents struct {
		Incidents []store.SupervisorRun `json:"incidents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&incidents); err != nil {
		t.Fatal(err)
	}
	if len(incidents.Incidents) != 1 || incidents.Incidents[0].Diagnosis != "stale heartbeat" {
		t.Fatalf("incidents = %+v", incidents.Incidents)
	}

	resp = env.do(t, http.MethodGet, "/api/v1/supervisor/costs?batch_id=api-batch", token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cost endpoint = %d", resp.StatusCode)
	}
	var costs store.BatchCostSummary
	if err := json.NewDecoder(resp.Body).Decode(&costs); err != nil {
		t.Fatal(err)
	}
	if costs.BatchID != "api-batch" || costs.OverallReportedUSD != 0 {
		t.Fatalf("costs = %+v", costs)
	}

	resp = env.do(t, http.MethodPost, "/api/v1/books/"+jsonNumber(book.ID)+"/ask-supervisor", token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("manual model gate = %d, want 409", resp.StatusCode)
	}
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
