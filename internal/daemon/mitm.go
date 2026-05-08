package daemon

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
)

// ProviderLaunchEnvironment returns daemon-owned provider launch environment.
func (s *Server) ProviderLaunchEnvironment(ctx context.Context, req *clydev1.ProviderLaunchEnvironmentRequest) (*clydev1.ProviderLaunchEnvironmentResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	provider := strings.TrimSpace(req.GetProvider())
	if provider == "" {
		return nil, status.Error(codes.InvalidArgument, "provider is required")
	}
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load config: %v", err)
	}
	var env map[string]string
	switch provider {
	case "claude":
		env, err = mitm.ClaudeEnv(ctx, cfg.MITM, s.mitmProxy())
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported provider %q", provider)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider launch environment: %v", err)
	}
	resp := &clydev1.ProviderLaunchEnvironmentResponse{}
	for key, value := range env {
		resp.Environment = append(resp.Environment, &clydev1.EnvironmentVariable{
			Key:   key,
			Value: value,
		})
	}
	return resp, nil
}
