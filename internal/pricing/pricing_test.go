package pricing

import "testing"

func TestEstimateRequiresVersionedExplicitRate(t *testing.T) {
	table := Table{Version: "rates-v1", Rates: map[string]Rate{
		"codex/gpt-test": {InputUSDPerMillion: 2, OutputUSDPerMillion: 8, CachedInputUSDPerMillion: 1},
	}}
	got, ok := table.Estimate("codex", "gpt-test", 1_000_000, 500_000, 250_000)
	if !ok || got != 6.25 {
		t.Fatalf("Estimate = %v, %v; want 6.25, true", got, ok)
	}
	if _, ok := (Table{}).Estimate("codex", "gpt-test", 1, 1, 1); ok {
		t.Fatal("unconfigured pricing unexpectedly produced an estimate")
	}
}
