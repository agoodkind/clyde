package daemon

import (
	"context"
	"slices"
	"sync"
	"time"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/providerid"
)

// ProviderStats is the daemon's in-memory adapter activity summary.
type ProviderStats struct {
	Provider                   providerid.Provider
	ProviderDetail             string
	Requests                   int
	Inflight                   int
	Streaming                  int
	InputTokens                int64
	OutputTokens               int64
	CacheReadTokens            int64
	CacheCreationTokens        int64
	HitRatio                   float64
	EstimatedCostMicrocents    int64
	LastSeenUnix               int64
	Error                      string
	DerivedCacheCreationTokens int64
}

type providerStatsRecorder struct {
	mu                  sync.Mutex
	stats               map[string]ProviderStats
	generationStartedAt time.Time
	now                 func() time.Time
	// pricing prices each terminal event's recorded token counts into an
	// estimated dollar cost at read time. It is the single source of
	// dollar cost: precompute at request time was removed so the codex
	// path is priced from its recorded tokens exactly like anthropic.
	pricing adapterruntime.PricingTable
}

func newProviderStatsRecorder(pricing adapterruntime.PricingTable) *providerStatsRecorder {
	return &providerStatsRecorder{
		mu:                  sync.Mutex{},
		stats:               make(map[string]ProviderStats),
		generationStartedAt: clock.Now(),
		now:                 clock.Now,
		pricing:             pricing,
	}
}

// replacePricing swaps the immutable pricing table under the same lock used
// for terminal-event cost calculation. A concurrent record call therefore
// observes either the complete old table or the complete new table.
func (r *providerStatsRecorder) replacePricing(pricing adapterruntime.PricingTable) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pricing = pricing
}

func (r *providerStatsRecorder) snapshot() ([]ProviderStats, time.Time) {
	if r == nil {
		return nil, time.Unix(0, 0)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ProviderStats, 0, len(r.stats))
	for _, stats := range r.stats {
		out = append(out, stats)
	}
	slices.SortFunc(out, func(a ProviderStats, b ProviderStats) int {
		if a.ProviderDetail < b.ProviderDetail {
			return -1
		}
		if a.ProviderDetail > b.ProviderDetail {
			return 1
		}
		return 0
	})
	return out, r.generationStartedAt
}

func (r *providerStatsRecorder) record(ctx context.Context, ev adapterruntime.RequestEvent) {
	_ = ctx
	if r == nil {
		return
	}
	providerDetail := ev.Provider
	if providerDetail == "" {
		providerDetail = ev.Backend
	}
	if providerDetail == "" {
		providerDetail = "unknown"
	}
	provider := providerid.MustParse(providerDetail)
	r.mu.Lock()
	defer r.mu.Unlock()
	stats := r.stats[providerDetail]
	stats.Provider = provider
	stats.ProviderDetail = providerDetail
	stats.LastSeenUnix = r.now().Unix()
	switch ev.Stage {
	case adapterruntime.RequestStageStarted:
		stats.Requests++
		stats.Inflight++
	case adapterruntime.RequestStageStreamOpened:
		stats.Streaming++
	case adapterruntime.RequestStageCompleted:
		stats = r.applyTerminalStats(stats, ev)
	case adapterruntime.RequestStageFailed, adapterruntime.RequestStageCancelled:
		stats = r.applyTerminalStats(stats, ev)
		stats.Error = ev.Err
	default:
	}
	r.stats[providerDetail] = stats
}

// applyTerminalStats folds one terminal event into the running provider
// stats. Dollar cost is computed here at read time by pricing the
// event's recorded token and cache counts against the configured
// PricingTable, then summing breakdown.TotalMicrocents. This is the
// single dollar source: it prices anthropic and codex events identically
// from their recorded tokens, which fixes the prior codex cost=0 bug
// where the codex dispatch path emitted CostMicrocents: 0 and the
// daemon never counted codex spend.
func (r *providerStatsRecorder) applyTerminalStats(stats ProviderStats, ev adapterruntime.RequestEvent) ProviderStats {
	if stats.Inflight > 0 {
		stats.Inflight--
	}
	if ev.Stream && stats.Streaming > 0 {
		stats.Streaming--
	}
	stats.InputTokens += int64(ev.TokensIn)
	stats.OutputTokens += int64(ev.TokensOut)
	stats.CacheReadTokens += int64(ev.CacheReadTokens)
	stats.CacheCreationTokens += int64(ev.CacheCreationTokens)
	stats.DerivedCacheCreationTokens += int64(ev.DerivedCacheCreationTokens)
	breakdown := adapterruntime.EstimateCost(adapterruntime.CostInputs{
		ModelID:             ev.ModelID,
		TTL:                 "",
		InputTokens:         ev.TokensIn,
		OutputTokens:        ev.TokensOut,
		CacheCreationTokens: ev.CacheCreationTokens,
		CacheReadTokens:     ev.CacheReadTokens,
	}, r.pricing)
	stats.EstimatedCostMicrocents += breakdown.TotalMicrocents
	// TODO(prong2 rate-limit snapshot): record a per-request rate-limit
	// snapshot here. Codex parses rate_limit windows in
	// internal/adapter/codex/usage_warning.go and anthropic parses
	// anthropic-ratelimit-unified-* headers in
	// internal/adapter/anthropic/notice.go, but both currently feed the
	// usage-notice subsystem rather than a snapshot carried on the
	// RequestEvent/Result. Threading that snapshot through is net-new
	// typed plumbing, deferred out of this read-time-cost slice.
	denominator := stats.InputTokens + stats.CacheReadTokens
	if denominator > 0 {
		stats.HitRatio = float64(stats.CacheReadTokens) / float64(denominator)
	}
	return stats
}
