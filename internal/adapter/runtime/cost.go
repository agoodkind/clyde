package runtime

import (
	"math"
	"strings"

	"goodkind.io/clyde/internal/config"
)

// dollarsPerMTokToMicrocentsPerToken converts a dollars-per-million-tokens
// list price into microcents-per-token. A microcent is 1e-6 of one US
// cent. One million tokens at $X.YY costs X.YY * 100 cents = X.YY * 100 *
// 1_000_000 microcents over 1_000_000 tokens, which is X.YY * 100
// microcents per token. Rounding to the nearest integer keeps the table
// in the integer domain so summing millions of requests in the
// aggregation pipeline never drifts.
func dollarsPerMTokToMicrocentsPerToken(dollarsPerMTok float64) int64 {
	if dollarsPerMTok <= 0 {
		return 0
	}
	return int64(math.Round(dollarsPerMTok * 100))
}

// modelRates captures the public per-token billing rates for one model,
// in microcents per token. Using integers avoids floating-point drift
// when summing millions of requests in aggregation pipelines.
//
// These rates assume the standard Messages API list price. They do NOT
// represent what the user is billed when running against a Max
// subscription bucket; they are the reference rates used to compare
// costs across paths so the user can pick the cheapest effective route.
type modelRates struct {
	InputPerToken      int64
	OutputPerToken     int64
	CacheWritePerToken int64
	CacheReadPerToken  int64
}

// PricingTable is the read-time rate table the cost calculator consumes.
// It holds microcents-per-token rates keyed by wire model id. Lookups
// match the recorded model id against the keys by longest-prefix so a
// single "claude-opus-4-8" entry covers "claude-opus-4-8[1m]" and
// snapshot variants. The table is built once from the config pricing map
// at load and threaded to the aggregator; it is read-only afterward.
type PricingTable struct {
	rates map[string]modelRates
}

// NewPricingTable builds a [PricingTable] from the config-supplied
// dollars-per-MTok pricing map. Each entry's dollar rates are converted
// to microcents-per-token. A nil or empty map yields a table whose
// lookups always miss, which the calculator treats as "unknown model,
// no estimate".
func NewPricingTable(pricing map[string]config.AdapterModelPricing) PricingTable {
	rates := make(map[string]modelRates, len(pricing))
	for modelID, p := range pricing {
		key := strings.ToLower(strings.TrimSpace(modelID))
		if key == "" {
			continue
		}
		rates[key] = modelRates{
			InputPerToken:      dollarsPerMTokToMicrocentsPerToken(p.InputPerMTok),
			OutputPerToken:     dollarsPerMTokToMicrocentsPerToken(p.OutputPerMTok),
			CacheWritePerToken: dollarsPerMTokToMicrocentsPerToken(p.CacheWritePerMTok),
			CacheReadPerToken:  dollarsPerMTokToMicrocentsPerToken(p.CacheReadPerMTok),
		}
	}
	return PricingTable{rates: rates}
}

// lookup returns the rates for the given model id, matched by
// longest-prefix: "claude-opus-4-8[1m]" matches a "claude-opus-4-8" key.
// The second return is false when nothing matches; callers treat a miss
// as "unknown model, skip estimate".
func (t PricingTable) lookup(modelID string) (modelRates, bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" || len(t.rates) == 0 {
		return modelRates{InputPerToken: 0, OutputPerToken: 0, CacheWritePerToken: 0, CacheReadPerToken: 0}, false
	}
	var bestKey string
	for k := range t.rates {
		if strings.HasPrefix(id, k) && len(k) > len(bestKey) {
			bestKey = k
		}
	}
	if bestKey == "" {
		return modelRates{InputPerToken: 0, OutputPerToken: 0, CacheWritePerToken: 0, CacheReadPerToken: 0}, false
	}
	return t.rates[bestKey], true
}

// CostInputs is part of Clyde's typed adapter surface.
type CostInputs struct {
	ModelID             string
	TTL                 string // "" / "5m" / "1h"
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

// CostBreakdown is part of Clyde's typed adapter surface.
type CostBreakdown struct {
	InputMicrocents      int64
	OutputMicrocents     int64
	CacheWriteMicrocents int64
	CacheReadMicrocents  int64
	TotalMicrocents      int64
	// HypotheticalNoCacheMicrocents is the cost if every cache-read
	// token had been billed as a fresh input token. Useful for
	// quantifying cache savings.
	HypotheticalNoCacheMicrocents int64
	// CacheSavingsMicrocents = Hypothetical - Total.
	CacheSavingsMicrocents int64
	// RatesKnown is true only when the model id matched an entry in the
	// pricing table.
	RatesKnown bool
}

// EstimateCost prices the recorded token counts against the supplied
// pricing table. It returns a breakdown with RatesKnown=false and all
// zero costs when the model id does not match any table entry. The TTL
// argument is accepted for forward compatibility with per-TTL cache
// rates; the config table currently carries a single cache-write rate,
// so TTL does not change the result today.
func EstimateCost(in CostInputs, rates PricingTable) CostBreakdown {
	r, ok := rates.lookup(in.ModelID)
	if !ok {
		return CostBreakdown{RatesKnown: false, InputMicrocents: 0, OutputMicrocents: 0, CacheWriteMicrocents: 0, CacheReadMicrocents: 0, TotalMicrocents: 0, HypotheticalNoCacheMicrocents: 0, CacheSavingsMicrocents: 0}
	}
	b := CostBreakdown{
		InputMicrocents:      int64(in.InputTokens) * r.InputPerToken,
		OutputMicrocents:     int64(in.OutputTokens) * r.OutputPerToken,
		CacheWriteMicrocents: int64(in.CacheCreationTokens) * r.CacheWritePerToken,
		CacheReadMicrocents:  int64(in.CacheReadTokens) * r.CacheReadPerToken,
		RatesKnown:           true, TotalMicrocents: 0, HypotheticalNoCacheMicrocents: 0, CacheSavingsMicrocents: 0,
	}
	b.TotalMicrocents = b.InputMicrocents + b.OutputMicrocents + b.CacheWriteMicrocents + b.CacheReadMicrocents
	// Had the cache-read tokens been fresh input, they would have cost
	// the full input rate instead of the cache-read rate. The write
	// surcharge was paid on the first turn, which we don't subtract
	// here; this measures marginal savings on reads only.
	noCacheInput := int64(in.InputTokens+in.CacheReadTokens) * r.InputPerToken
	b.HypotheticalNoCacheMicrocents = noCacheInput + b.OutputMicrocents + b.CacheWriteMicrocents
	b.CacheSavingsMicrocents = b.HypotheticalNoCacheMicrocents - b.TotalMicrocents
	return b
}
