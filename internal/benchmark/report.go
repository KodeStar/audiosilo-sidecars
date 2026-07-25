package benchmark

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
)

type AggregateReport struct {
	Version    int                `json:"version"`
	Generated  string             `json:"generated_at"`
	Profiles   []ProfileAggregate `json:"profiles"`
	Pareto     []string           `json:"pareto_frontier"`
	MethodNote string             `json:"method_note"`
}

type ProfileAggregate struct {
	Profile               string  `json:"profile"`
	Runs                  int     `json:"runs"`
	Completed             int     `json:"completed"`
	ValidationPassed      int     `json:"validation_passed"`
	HoldoutRuns           int     `json:"holdout_runs"`
	HoldoutPassed         int     `json:"holdout_passed"`
	MeanWallSeconds       float64 `json:"mean_wall_seconds"`
	MedianWallSeconds     float64 `json:"median_wall_seconds"`
	MeanAgentSeconds      float64 `json:"mean_agent_seconds"`
	MedianAgentSeconds    float64 `json:"median_agent_seconds"`
	MeanInputTokens       float64 `json:"mean_input_tokens"`
	MeanOutputTokens      float64 `json:"mean_output_tokens"`
	MeanCacheReadTokens   float64 `json:"mean_cache_read_tokens"`
	MeanReportedCostUSD   float64 `json:"mean_provider_reported_cost_usd"`
	ReportedCostComplete  bool    `json:"provider_reported_cost_complete"`
	ReportedCostAvailable bool    `json:"provider_reported_cost_available"`
	MeanEstimatedCostUSD  float64 `json:"mean_estimated_api_cost_usd"`
	CostEstimateComplete  bool    `json:"cost_estimate_complete"`
	MeanCharacterRecall   float64 `json:"mean_character_name_recall"`
	MeanRecapJaccard      float64 `json:"mean_recap_point_jaccard"`
	MeanAuditRounds       float64 `json:"mean_audit_rounds"`
	MeanFixRounds         float64 `json:"mean_fix_rounds"`
	ValidationFailures    int     `json:"validation_failed_invocations"`
	InvocationFailures    int     `json:"failed_invocations"`
}

func BuildReport(resultsRoot string, now time.Time) (AggregateReport, error) {
	var runs []RunResult
	err := filepath.WalkDir(resultsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "result.json" {
			return nil
		}
		raw, err := os.ReadFile(path) //nolint:gosec // walked benchmark result root
		if err != nil {
			return err
		}
		var r RunResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		runs = append(runs, r)
		return nil
	})
	if err != nil {
		return AggregateReport{}, err
	}
	if len(runs) == 0 {
		return AggregateReport{}, fmt.Errorf("no result.json files under %s", resultsRoot)
	}

	byProfile := map[string][]RunResult{}
	for _, r := range runs {
		byProfile[r.Profile] = append(byProfile[r.Profile], r)
	}
	ids := make([]string, 0, len(byProfile))
	for id := range byProfile {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	report := AggregateReport{Version: schemaVersion, Generated: now.UTC().Format(time.RFC3339),
		MethodNote: "Quality gates are primary. Reference-name and recap-point overlap are stability diagnostics, not semantic correctness scores. Provider-reported generation cost is shown separately from the versioned API-equivalent estimate; holdout-judge spend is excluded from both."}
	for _, id := range ids {
		report.Profiles = append(report.Profiles, aggregate(id, byProfile[id]))
	}
	report.Pareto = pareto(report.Profiles)
	return report, nil
}

func aggregate(id string, runs []RunResult) ProfileAggregate {
	a := ProfileAggregate{Profile: id, Runs: len(runs), ReportedCostComplete: true, CostEstimateComplete: true}
	var wall, input, output, cache, reportedCost, estimatedCost, charRecall, recap, audits, fixes float64
	wallSamples := make([]float64, 0, len(runs))
	agentSamples := make([]float64, 0, len(runs))
	for _, r := range runs {
		if r.Completed {
			a.Completed++
		}
		if r.Quality.ValidationClean {
			a.ValidationPassed++
		}
		for _, holdout := range r.Holdouts {
			a.HoldoutRuns++
			if holdout.Passed {
				a.HoldoutPassed++
			}
		}
		wall += r.WallSeconds
		wallSamples = append(wallSamples, r.WallSeconds)
		runAgentSeconds := 0.0
		charRecall += r.Quality.CharacterRecall
		recap += r.Quality.RecapPointJaccard
		audits += float64(r.AuditRounds)
		fixes += float64(r.FixRounds)
		for _, v := range r.Invocations {
			input += float64(v.InputTokens)
			output += float64(v.OutputTokens)
			cache += float64(v.CacheReadTokens)
			runAgentSeconds += elapsed(v.StartedAt, v.CompletedAt)
			if v.CostReported {
				reportedCost += v.CostUSD
				a.ReportedCostAvailable = true
			} else if v.InputTokens+v.OutputTokens+v.CacheReadTokens > 0 {
				a.ReportedCostComplete = false
			}
			if v.EstimatedAPICostUSD != nil {
				estimatedCost += *v.EstimatedAPICostUSD
			} else if v.InputTokens+v.OutputTokens+v.CacheReadTokens > 0 {
				a.CostEstimateComplete = false
			}
			if v.Status == "validation_failed" {
				a.ValidationFailures++
			}
			if v.Status == "failure" || v.Status == "cancelled" {
				a.InvocationFailures++
			}
		}
		agentSamples = append(agentSamples, runAgentSeconds)
	}
	n := float64(len(runs))
	a.MeanWallSeconds = wall / n
	a.MedianWallSeconds = median(wallSamples)
	for _, seconds := range agentSamples {
		a.MeanAgentSeconds += seconds / n
	}
	a.MedianAgentSeconds = median(agentSamples)
	a.MeanInputTokens = input / n
	a.MeanOutputTokens = output / n
	a.MeanCacheReadTokens = cache / n
	a.MeanReportedCostUSD = reportedCost / n
	if !a.ReportedCostAvailable {
		a.ReportedCostComplete = false
	}
	a.MeanEstimatedCostUSD = estimatedCost / n
	a.MeanCharacterRecall = charRecall / n
	a.MeanRecapJaccard = recap / n
	a.MeanAuditRounds = audits / n
	a.MeanFixRounds = fixes / n
	return a
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	mid := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[mid]
	}
	return (ordered[mid-1] + ordered[mid]) / 2
}

func elapsed(start, finish string) float64 {
	if finish == "" {
		return 0
	}
	a, err1 := time.Parse(time.RFC3339Nano, start)
	b, err2 := time.Parse(time.RFC3339Nano, finish)
	if err1 != nil || err2 != nil || b.Before(a) {
		return 0
	}
	return b.Sub(a).Seconds()
}

// pareto returns hard-gate-eligible profiles not strictly dominated on reference
// stability, wall time, and estimated cost. An incomplete cost estimate is
// incomparable and therefore cannot dominate a fully priced profile.
func pareto(all []ProfileAggregate) []string {
	var out []string
	for i, p := range all {
		if !hardGateEligible(p) {
			continue
		}
		dominated := false
		for j, q := range all {
			if i == j || !hardGateEligible(q) || (p.CostEstimateComplete && !q.CostEstimateComplete) {
				continue
			}
			qualityNoWorse := q.MeanCharacterRecall >= p.MeanCharacterRecall && q.MeanRecapJaccard >= p.MeanRecapJaccard
			resourceNoWorse := q.MeanWallSeconds <= p.MeanWallSeconds && (!p.CostEstimateComplete || q.MeanEstimatedCostUSD <= p.MeanEstimatedCostUSD)
			strict := q.MeanWallSeconds < p.MeanWallSeconds || (p.CostEstimateComplete && q.MeanEstimatedCostUSD < p.MeanEstimatedCostUSD) || q.MeanCharacterRecall > p.MeanCharacterRecall || q.MeanRecapJaccard > p.MeanRecapJaccard
			if qualityNoWorse && resourceNoWorse && strict {
				dominated = true
				break
			}
		}
		if !dominated {
			out = append(out, p.Profile)
		}
	}
	sort.Strings(out)
	return out
}

func hardGateEligible(p ProfileAggregate) bool {
	return p.Runs > 0 && p.Completed == p.Runs && p.ValidationPassed == p.Runs && p.HoldoutRuns > 0 && p.HoldoutPassed == p.HoldoutRuns
}

func WriteReport(root string, report AggregateReport) error {
	if err := writeJSON(filepath.Join(root, "report.json"), report); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Sidecar agent benchmark\n\n")
	b.WriteString("Quality gates are pass/fail. Name recall and recap-point overlap measure stability against the accepted reference, not factual correctness. Holdout audits are fresh sessions and should use a different provider when available.\n\n")
	b.WriteString("| Profile | Complete | Validation | Holdout | Wall min mean/median | Agent min mean/median | Input M | Output k | Reported gen cost | API proxy | Character recall | Recap overlap | Audit rounds | Fix rounds |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, p := range report.Profiles {
		reportedCost := "unknown"
		if p.ReportedCostAvailable && p.ReportedCostComplete {
			reportedCost = fmt.Sprintf("$%.2f", p.MeanReportedCostUSD)
		}
		estimatedCost := "unknown"
		if p.CostEstimateComplete {
			estimatedCost = fmt.Sprintf("$%.2f", p.MeanEstimatedCostUSD)
		}
		fmt.Fprintf(&b, "| %s | %d/%d | %d/%d | %d/%d | %.1f / %.1f | %.1f / %.1f | %.2f | %.0f | %s | %s | %.0f%% | %.0f%% | %.2f | %.2f |\n",
			p.Profile, p.Completed, p.Runs, p.ValidationPassed, p.Runs, p.HoldoutPassed, p.HoldoutRuns, p.MeanWallSeconds/60, p.MedianWallSeconds/60, p.MeanAgentSeconds/60, p.MedianAgentSeconds/60, p.MeanInputTokens/1e6, p.MeanOutputTokens/1e3, reportedCost, estimatedCost, p.MeanCharacterRecall*100, p.MeanRecapJaccard*100, p.MeanAuditRounds, p.MeanFixRounds)
	}
	if len(report.Pareto) == 0 {
		b.WriteString("\nPareto frontier: none; no measured profile passed every hard gate.\n")
	} else {
		b.WriteString("\nPareto frontier: " + strings.Join(report.Pareto, ", ") + ".\n")
	}
	return fsutil.WriteFileAtomic(filepath.Join(root, "report.md"), []byte(b.String()), 0o644)
}
