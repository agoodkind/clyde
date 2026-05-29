package runtime

import (
	"testing"

	"goodkind.io/clyde/internal/config"
)

// sonnet4Pricing returns a config pricing fixture whose dollar rates
// reproduce the former hardcoded sonnet-4 microcents-per-token table:
// $3/$15 in/out, $3.75 cache write, $0.30 cache read.
func sonnet4Pricing() map[string]config.AdapterModelPricing {
	return map[string]config.AdapterModelPricing{
		"claude-sonnet-4": {
			InputPerMTok:      3,
			OutputPerMTok:     15,
			CacheWritePerMTok: 3.75,
			CacheReadPerMTok:  0.30,
		},
	}
}

func TestNewPricingTableConvertsDollarsToMicrocents(t *testing.T) {
	table := NewPricingTable(sonnet4Pricing())
	rates, ok := table.lookup("claude-sonnet-4-6")
	if !ok {
		t.Fatalf("expected longest-prefix match for claude-sonnet-4-6")
	}
	// $3/MTok -> 300 microcents/token, $15 -> 1500, $3.75 -> 375, $0.30 -> 30.
	if rates.InputPerToken != 300 {
		t.Errorf("input rate=%d want 300", rates.InputPerToken)
	}
	if rates.OutputPerToken != 1500 {
		t.Errorf("output rate=%d want 1500", rates.OutputPerToken)
	}
	if rates.CacheWritePerToken != 375 {
		t.Errorf("cache write rate=%d want 375", rates.CacheWritePerToken)
	}
	if rates.CacheReadPerToken != 30 {
		t.Errorf("cache read rate=%d want 30", rates.CacheReadPerToken)
	}
	if _, ok := table.lookup("unknown-model"); ok {
		t.Fatalf("lookup for unknown model should miss")
	}
}

func TestEmptyPricingTableMisses(t *testing.T) {
	table := NewPricingTable(nil)
	if _, ok := table.lookup("claude-sonnet-4"); ok {
		t.Fatalf("empty table must always miss")
	}
	b := EstimateCost(CostInputs{ModelID: "claude-sonnet-4", TTL: "", InputTokens: 100, OutputTokens: 50, CacheCreationTokens: 0, CacheReadTokens: 0}, table)
	if b.RatesKnown {
		t.Fatalf("rates must be unknown with an empty table")
	}
	if b.TotalMicrocents != 0 {
		t.Fatalf("unknown rates must yield zero cost, got %d", b.TotalMicrocents)
	}
}

func TestEstimateCostPricesRecordedTokens(t *testing.T) {
	table := NewPricingTable(sonnet4Pricing())
	in := CostInputs{
		ModelID:             "claude-sonnet-4",
		TTL:                 "",
		InputTokens:         1000,
		OutputTokens:        500,
		CacheCreationTokens: 200,
		CacheReadTokens:     800,
	}
	b := EstimateCost(in, table)
	if !b.RatesKnown {
		t.Fatalf("rates should be known for sonnet-4")
	}
	// Input: 1000 * 300 = 300_000 microcents
	// Output: 500 * 1500 = 750_000
	// Cache write: 200 * 375 = 75_000
	// Cache read: 800 * 30 = 24_000
	// Total: 1_149_000
	if b.TotalMicrocents != 1_149_000 {
		t.Fatalf("unexpected total microcents: %d", b.TotalMicrocents)
	}
}

func TestEstimateCostSavingsVsNoCache(t *testing.T) {
	table := NewPricingTable(sonnet4Pricing())
	in := CostInputs{
		ModelID:             "claude-sonnet-4",
		TTL:                 "",
		InputTokens:         100,
		OutputTokens:        10,
		CacheCreationTokens: 0,
		CacheReadTokens:     10_000,
	}
	b := EstimateCost(in, table)
	// No-cache hypothetical adds 10_000 input tokens at full rate.
	// Actual billed read at the cache-read rate.
	// Savings = (10_000 * 300) - (10_000 * 30) = 2_700_000 microcents.
	if b.CacheSavingsMicrocents != 2_700_000 {
		t.Fatalf("unexpected savings: %d", b.CacheSavingsMicrocents)
	}
}

// TestEstimateCostOpus48AndContextVariant is the Track C anchor case:
// a config fixture at $5/$25 in/out (500 / 2500 microcents-per-token)
// must price both "claude-opus-4-8" and its context-suffixed
// "claude-opus-4-8[1m]" variant via longest-prefix match.
func TestEstimateCostOpus48AndContextVariant(t *testing.T) {
	table := NewPricingTable(map[string]config.AdapterModelPricing{
		"claude-opus-4-8": {
			InputPerMTok:      5,
			OutputPerMTok:     25,
			CacheWritePerMTok: 0,
			CacheReadPerMTok:  0,
		},
	})
	for _, modelID := range []string{"claude-opus-4-8", "claude-opus-4-8[1m]"} {
		b := EstimateCost(CostInputs{
			ModelID:             modelID,
			TTL:                 "",
			InputTokens:         1_000,
			OutputTokens:        1_000,
			CacheCreationTokens: 0,
			CacheReadTokens:     0,
		}, table)
		if !b.RatesKnown {
			t.Fatalf("rates should be known for %q", modelID)
		}
		// 1000 * 500 input + 1000 * 2500 output = 3_000_000 microcents.
		if b.InputMicrocents != 500_000 {
			t.Errorf("%q input microcents=%d want 500_000", modelID, b.InputMicrocents)
		}
		if b.OutputMicrocents != 2_500_000 {
			t.Errorf("%q output microcents=%d want 2_500_000", modelID, b.OutputMicrocents)
		}
		if b.TotalMicrocents != 3_000_000 {
			t.Errorf("%q total microcents=%d want 3_000_000", modelID, b.TotalMicrocents)
		}
	}
}

// TestEstimateCostCodexModelPricesNonzero is the codex cost=0 bug-fix
// regression: a configured codex model id prices to a nonzero read-time
// cost from its recorded tokens, exactly like anthropic.
func TestEstimateCostCodexModelPricesNonzero(t *testing.T) {
	table := NewPricingTable(map[string]config.AdapterModelPricing{
		"gpt-5": {
			InputPerMTok:      1.25,
			OutputPerMTok:     10,
			CacheWritePerMTok: 0,
			CacheReadPerMTok:  0.125,
		},
	})
	b := EstimateCost(CostInputs{
		ModelID:             "gpt-5",
		TTL:                 "",
		InputTokens:         10_000,
		OutputTokens:        2_000,
		CacheCreationTokens: 0,
		CacheReadTokens:     5_000,
	}, table)
	if !b.RatesKnown {
		t.Fatalf("rates should be known for configured codex model gpt-5")
	}
	if b.TotalMicrocents <= 0 {
		t.Fatalf("codex model must price to a nonzero cost, got %d", b.TotalMicrocents)
	}
	// Input: 10_000 * 125 = 1_250_000
	// Output: 2_000 * 1000 = 2_000_000
	// Cache read: 5_000 * 12.5 -> 5_000 * 13 (rounded) = 65_000
	// $1.25/MTok -> round(125) = 125; $0.125/MTok -> round(12.5) = 13.
	if b.InputMicrocents != 1_250_000 {
		t.Errorf("input microcents=%d want 1_250_000", b.InputMicrocents)
	}
	if b.OutputMicrocents != 2_000_000 {
		t.Errorf("output microcents=%d want 2_000_000", b.OutputMicrocents)
	}
	if b.CacheReadMicrocents != 65_000 {
		t.Errorf("cache read microcents=%d want 65_000", b.CacheReadMicrocents)
	}
}
