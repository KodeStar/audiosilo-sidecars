package benchmark

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"

	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

type HistoryReport struct {
	Version     int                `json:"version"`
	Generated   string             `json:"generated_at"`
	Books       int                `json:"books"`
	DoneBooks   int                `json:"done_books"`
	Invocations int                `json:"invocations"`
	Groups      []HistoryAggregate `json:"groups"`
}

type HistoryAggregate struct {
	Stage              string  `json:"stage"`
	Backend            string  `json:"backend"`
	Model              string  `json:"model"`
	Invocations        int     `json:"invocations"`
	Successes          int     `json:"successes"`
	ValidationFailures int     `json:"validation_failures"`
	Failures           int     `json:"failures"`
	MeanSeconds        float64 `json:"mean_seconds"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	CacheReadTokens    int64   `json:"cache_read_tokens"`
	EstimatedCostUSD   float64 `json:"estimated_api_cost_usd"`
	EstimateComplete   bool    `json:"estimate_complete"`
}

// BuildHistory summarizes a DATABASE SNAPSHOT. Callers should never point it at the
// live daemon DB: store.Open applies migrations and is intentionally a writable API.
func BuildHistory(ctx context.Context, dbSnapshot string, now time.Time) (HistoryReport, error) {
	db, err := store.Open(ctx, dbSnapshot)
	if err != nil {
		return HistoryReport{}, err
	}
	defer func() { _ = db.Close() }()
	books, err := db.ListBooks(ctx)
	if err != nil {
		return HistoryReport{}, err
	}
	r := HistoryReport{Version: schemaVersion, Generated: now.UTC().Format(time.RFC3339), Books: len(books)}
	for _, b := range books {
		if b.State == "done" {
			r.DoneBooks++
		}
	}
	invocations, err := db.AgentInvocationsAll(ctx)
	if err != nil {
		return HistoryReport{}, err
	}
	type key struct{ stage, backend, model string }
	groups := map[key]*HistoryAggregate{}
	duration := map[key]float64{}
	durationCount := map[key]int{}
	for _, v := range invocations {
		r.Invocations++
		k := key{v.Stage, v.Backend, v.Model}
		g := groups[k]
		if g == nil {
			g = &HistoryAggregate{Stage: v.Stage, Backend: v.Backend, Model: v.Model, EstimateComplete: true}
			groups[k] = g
		}
		g.Invocations++
		g.InputTokens += v.InputTokens
		g.OutputTokens += v.OutputTokens
		g.CacheReadTokens += v.CacheReadTokens
		if v.CompletedAt != "" {
			duration[k] += elapsed(v.StartedAt, v.CompletedAt)
			durationCount[k]++
		}
		switch v.Status {
		case "success":
			g.Successes++
		case "validation_failed":
			g.ValidationFailures++
		case "failure", "cancelled":
			g.Failures++
		}
		if v.EstimatedAPICostUSD != nil {
			g.EstimatedCostUSD += *v.EstimatedAPICostUSD
		} else if v.InputTokens+v.OutputTokens+v.CacheReadTokens > 0 {
			g.EstimateComplete = false
		}
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].stage != keys[j].stage {
			return keys[i].stage < keys[j].stage
		}
		if keys[i].backend != keys[j].backend {
			return keys[i].backend < keys[j].backend
		}
		return keys[i].model < keys[j].model
	})
	for _, k := range keys {
		g := groups[k]
		if durationCount[k] > 0 {
			g.MeanSeconds = duration[k] / float64(durationCount[k])
		}
		r.Groups = append(r.Groups, *g)
	}
	return r, nil
}

// WriteHistory persists both machine-readable and human-readable telemetry with
// the benchmark package's standard private JSON permissions.
func WriteHistory(root string, report HistoryReport) error {
	if err := writeJSON(filepath.Join(root, "history.json"), report); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(root, "history.md"), []byte(HistoryMarkdown(report)), 0o644)
}

func HistoryMarkdown(r HistoryReport) string {
	s := fmt.Sprintf("# Historical agent telemetry\n\n%d books (%d done), %d concrete invocations. This is observational production data, not a randomized comparison.\n\n", r.Books, r.DoneBooks, r.Invocations)
	s += "| Stage | Backend/model | Calls | Success | Validation fail | Other fail | Mean sec | Input M | Output k | Cost proxy |\n|---|---|---:|---:|---:|---:|---:|---:|---:|---:|\n"
	for _, g := range r.Groups {
		cost := "unknown"
		if g.EstimateComplete {
			cost = fmt.Sprintf("$%.2f", g.EstimatedCostUSD)
		}
		s += fmt.Sprintf("| %s | %s/%s | %d | %d | %d | %d | %.1f | %.2f | %.0f | %s |\n", g.Stage, g.Backend, g.Model, g.Invocations, g.Successes, g.ValidationFailures, g.Failures, g.MeanSeconds, float64(g.InputTokens)/1e6, float64(g.OutputTokens)/1e3, cost)
	}
	return s
}
