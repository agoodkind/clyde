package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/providers/mitmcontrib"
	"goodkind.io/clyde/internal/session"
)

// ProviderLaunchEnvironment returns daemon-owned provider launch environment.
func (s *Server) ProviderLaunchEnvironment(ctx context.Context, req *clydev1.ProviderLaunchEnvironmentRequest) (*clydev1.ProviderLaunchEnvironmentResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	rawProvider := strings.TrimSpace(req.GetProvider())
	if rawProvider == "" {
		return nil, status.Error(codes.InvalidArgument, "provider is required")
	}
	provider := session.NormalizeProviderID(session.ProviderID(rawProvider))
	if provider == session.ProviderUnknown {
		return nil, status.Error(codes.InvalidArgument, "provider is required")
	}
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load config: %v", err)
	}
	env, err := providerLaunchEnvironmentFromRegistry(ctx, s.log, provider, cfg, s.mitmProxy())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider launch environment: %v", err)
	}
	reauth, err := s.applyRotatorLaunchCredentials(ctx, provider, cfg, &env)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "rotator launch credentials: %v", err)
	}
	resp := &clydev1.ProviderLaunchEnvironmentResponse{Reauth: reauth}
	for key, value := range env {
		resp.Environment = append(resp.Environment, &clydev1.EnvironmentVariable{
			Key:   key,
			Value: value,
		})
	}
	return resp, nil
}

// providerLaunchEnvironmentFromRegistry resolves the registered
// Contributor for provider and returns its env. Generic code in
// internal/daemon does not import provider env constants directly:
// each provider package registers its Contributor in init.
func providerLaunchEnvironmentFromRegistry(ctx context.Context, log *slog.Logger, provider session.ProviderID, cfg *config.Config, proxy *mitm.Proxy) (map[string]string, error) {
	contributor, ok := mitmcontrib.Get(string(provider))
	if !ok {
		if log != nil {
			log.WarnContext(ctx, "daemon.provider_launch_env.unregistered",
				"component", "daemon",
				"provider", provider,
			)
		}
		return nil, nil
	}
	env, err := contributor.Env(ctx, cfg, proxy)
	if err != nil {
		return nil, fmt.Errorf("contributor env for %q: %w", provider, err)
	}
	return env, nil
}
