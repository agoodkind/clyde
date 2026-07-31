package daemon

import (
	"context"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
)

// ProviderStatsSnapshot is one live provider-counter generation from the daemon.
type ProviderStatsSnapshot struct {
	Providers    []ProviderStats
	LoadedAtUnix int64
}

// CurrentProviderStats reads the daemon's current provider counters once.
func CurrentProviderStats(ctx context.Context) (ProviderStatsSnapshot, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return ProviderStatsSnapshot{}, err
	}
	defer func() { _ = client.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	response, err := client.rpc.GetProviderStats(rpcCtx, &clydev1.GetProviderStatsRequest{})
	if err != nil {
		return ProviderStatsSnapshot{}, daemonRPCError(rpcCtx, "get provider stats", err)
	}
	providers := make([]ProviderStats, 0, len(response.GetProviders()))
	for _, stats := range response.GetProviders() {
		providers = append(providers, ProviderStats{
			Provider: providerFromProto(stats.GetProvider()), ProviderDetail: stats.GetProviderDetail(), Requests: int(stats.GetRequests()),
			Inflight: int(stats.GetInflight()), Streaming: int(stats.GetStreaming()), InputTokens: stats.GetInputTokens(),
			OutputTokens: stats.GetOutputTokens(), CacheReadTokens: stats.GetCacheReadTokens(), CacheCreationTokens: stats.GetCacheCreationTokens(),
			HitRatio: stats.GetHitRatio(), EstimatedCostMicrocents: stats.GetEstimatedCostMicrocents(), LastSeenUnix: stats.GetLastSeenUnix(),
			Error: stats.GetError(), DerivedCacheCreationTokens: stats.GetDerivedCacheCreationTokens(),
		})
	}
	return ProviderStatsSnapshot{Providers: providers, LoadedAtUnix: response.GetLoadedAtUnix()}, nil
}
