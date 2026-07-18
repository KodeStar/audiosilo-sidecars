// Package pricing estimates API-equivalent model cost from a versioned operator
// supplied rate table. It never treats a missing provider cost as zero spend.
package pricing

import "strings"

// Rate is USD per one million tokens for a backend/model pair.
type Rate struct {
	InputUSDPerMillion       float64 `yaml:"input_usd_per_million" json:"input_usd_per_million"`
	OutputUSDPerMillion      float64 `yaml:"output_usd_per_million" json:"output_usd_per_million"`
	CachedInputUSDPerMillion float64 `yaml:"cached_input_usd_per_million" json:"cached_input_usd_per_million"`
}

// Table is deliberately configuration data rather than hard-coded vendor prices.
// Rates keys are "backend/model"; "backend/*" is a permitted explicit fallback.
type Table struct {
	Version string          `yaml:"version" json:"version"`
	Rates   map[string]Rate `yaml:"rates" json:"rates"`
}

// Estimate returns an API-equivalent cost and true only when an explicit rate exists.
func (t Table) Estimate(backend, model string, input, output, cached int64) (float64, bool) {
	if t.Version == "" || len(t.Rates) == 0 {
		return 0, false
	}
	backend, model = strings.TrimSpace(backend), strings.TrimSpace(model)
	rate, ok := t.Rates[backend+"/"+model]
	if !ok {
		rate, ok = t.Rates[backend+"/*"]
	}
	if !ok {
		return 0, false
	}
	const million = 1_000_000.0
	return (float64(input)*rate.InputUSDPerMillion +
		float64(output)*rate.OutputUSDPerMillion +
		float64(cached)*rate.CachedInputUSDPerMillion) / million, true
}
