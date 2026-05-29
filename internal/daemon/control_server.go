package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/providerid"
)

const providerStatsStreamInterval = time.Second

type controlServer struct {
	clydev1.UnimplementedClydeServiceServer
	stats  *providerStatsRecorder
	reload func(context.Context) (*clydev1.ReloadDaemonResponse, error)
}

func (s *controlServer) ReloadDaemon(ctx context.Context, _ *clydev1.ReloadDaemonRequest) (*clydev1.ReloadDaemonResponse, error) {
	if s.reload == nil {
		return nil, status.Error(codes.FailedPrecondition, "daemon reload is not available")
	}
	return s.reload(ctx)
}

func (s *controlServer) GetProviderStats(context.Context, *clydev1.GetProviderStatsRequest) (*clydev1.GetProviderStatsResponse, error) {
	return providerStatsResponse(s.stats), nil
}

func (s *controlServer) SubscribeProviderStats(_ *clydev1.SubscribeProviderStatsRequest, stream grpc.ServerStreamingServer[clydev1.ProviderStatsEvent]) error {
	ticker := time.NewTicker(providerStatsStreamInterval)
	defer ticker.Stop()
	for {
		response := providerStatsResponse(s.stats)
		for _, stats := range response.GetProviders() {
			event := &clydev1.ProviderStatsEvent{
				Stats:         stats,
				EmittedAtUnix: response.GetLoadedAtUnix(),
			}
			if err := stream.Send(event); err != nil {
				slog.WarnContext(stream.Context(), "daemon.provider_stats.stream_send_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
					"err", err,
				)
				return fmt.Errorf("send provider stats event: %w", err)
			}
		}
		select {
		case <-stream.Context().Done():
			slog.WarnContext(stream.Context(), "daemon.provider_stats.stream_context_done", "concern", "process.daemon.lifecycle", "component", "daemon",
				"err", stream.Context().Err(),
			)
			return fmt.Errorf("provider stats stream context: %w", stream.Context().Err())
		case <-ticker.C:
		}
	}
}

func providerStatsResponse(recorder *providerStatsRecorder) *clydev1.GetProviderStatsResponse {
	snapshot, loadedAt := recorder.snapshot()
	out := make([]*clydev1.ProviderStats, 0, len(snapshot))
	for _, stats := range snapshot {
		out = append(out, &clydev1.ProviderStats{
			Provider:                   protoProvider(stats.Provider),
			Requests:                   int32FromInt(stats.Requests),
			Inflight:                   int32FromInt(stats.Inflight),
			Streaming:                  int32FromInt(stats.Streaming),
			InputTokens:                stats.InputTokens,
			OutputTokens:               stats.OutputTokens,
			CacheReadTokens:            stats.CacheReadTokens,
			CacheCreationTokens:        stats.CacheCreationTokens,
			HitRatio:                   stats.HitRatio,
			EstimatedCostMicrocents:    stats.EstimatedCostMicrocents,
			LastSeenUnix:               stats.LastSeenUnix,
			Error:                      stats.Error,
			DerivedCacheCreationTokens: stats.DerivedCacheCreationTokens,
			ProviderDetail:             stats.ProviderDetail,
		})
	}
	return &clydev1.GetProviderStatsResponse{
		Providers:    out,
		LoadedAtUnix: loadedAt.Unix(),
	}
}

func int32FromInt(value int) int32 {
	const maxInt32Value = 2147483647
	const minInt32Value = -2147483648
	if value > maxInt32Value {
		return maxInt32Value
	}
	if value < minInt32Value {
		return minInt32Value
	}
	return int32(value)
}

func protoProvider(provider providerid.Provider) clydev1.Provider {
	switch provider {
	case providerid.ProviderClaude:
		return clydev1.Provider_PROVIDER_CLAUDE
	case providerid.ProviderCodex:
		return clydev1.Provider_PROVIDER_CODEX
	case providerid.ProviderAnthropic:
		return clydev1.Provider_PROVIDER_ANTHROPIC
	case providerid.ProviderOpenAICompat:
		return clydev1.Provider_PROVIDER_OPENAI_COMPAT
	case providerid.ProviderMITM:
		return clydev1.Provider_PROVIDER_MITM
	case providerid.ProviderArtifact:
		return clydev1.Provider_PROVIDER_ARTIFACT
	case providerid.ProviderCursor:
		return clydev1.Provider_PROVIDER_CURSOR
	case providerid.ProviderUnspecified:
		return clydev1.Provider_PROVIDER_UNSPECIFIED
	default:
		return clydev1.Provider_PROVIDER_UNSPECIFIED
	}
}
