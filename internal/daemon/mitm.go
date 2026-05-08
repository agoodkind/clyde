package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/adapter"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
	codexprovider "goodkind.io/clyde/internal/providers/codex"
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
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load config: %v", err)
	}
	var env map[string]string
	switch provider {
	case session.ProviderUnknown:
		return nil, status.Error(codes.InvalidArgument, "provider is required")
	case session.ProviderClaude:
		env, err = mitm.ClaudeEnv(ctx, cfg.MITM, s.mitmProxy())
	case session.ProviderCodex:
		env, err = codexLaunchEnvironment(ctx, s.log, cfg, s.mitmProxy())
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

func codexLaunchEnvironment(ctx context.Context, log *slog.Logger, cfg *config.Config, proxy *mitm.Proxy) (map[string]string, error) {
	if cfg == nil {
		return nil, nil
	}
	port := cfg.Adapter.Port
	if port == 0 {
		port = adapter.DefaultPort
	}
	env := map[string]string{
		codexprovider.OpenAIBaseURLEnv: fmt.Sprintf("http://[::1]:%d/v1", port),
	}
	if !cfg.MITM.EnabledDefault || !cfg.MITM.EnabledFor("codex") {
		return env, nil
	}
	proxyEnv, err := mitm.CodexEnv(ctx, cfg.MITM, proxy)
	if err != nil {
		if log != nil {
			log.WarnContext(ctx, "daemon.codex.launch_env_failed",
				"component", "daemon",
				"provider", session.ProviderCodex,
				"err", err,
			)
		}
		return nil, fmt.Errorf("codex mitm environment: %w", err)
	}
	maps.Copy(env, proxyEnv)
	certPath := strings.TrimSpace(cfg.MITM.CA.CertPath)
	if certPath != "" {
		env[codexprovider.SSLCertFileEnv] = certPath
		env[codexprovider.NodeExtraCACertsEnv] = certPath
	}
	return env, nil
}
