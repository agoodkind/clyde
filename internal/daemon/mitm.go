package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
)

const (
	mitmProfileModeNormal   = "normal"
	mitmProfileModeIsolated = "isolated"
)

var (
	launchMITMUpstream = mitm.LaunchUpstream
	prepareMITMLaunch  = mitm.PrepareLaunch
)

// LaunchMITMUpstream asks the daemon to start a known upstream through
// Clyde's local MITM capture proxy.
func (s *Server) LaunchMITMUpstream(ctx context.Context, req *clydev1.LaunchMITMUpstreamRequest) (*clydev1.LaunchMITMUpstreamResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	preparation, err := prepareMITMLaunchOptions(
		req.GetUpstream(),
		req.GetProfileMode(),
		req.GetCaptureDir(),
		req.GetForce(),
		req.GetArgs(),
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	peerAddr := ""
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		peerAddr = p.Addr.String()
	}
	s.log.InfoContext(ctx, "daemon.mitm.launch_upstream",
		"component", "daemon",
		"upstream", preparation.upstream,
		"profile_mode", preparation.profileMode,
		"capture_dir", preparation.effectiveCaptureDir,
		"peer_addr", peerAddr,
		"force", req.GetForce(),
		"extra_arg_count", len(req.GetArgs()),
	)
	if err := launchMITMUpstream(ctx, mitm.LaunchUpstreamOptions{
		Profile:        preparation.profile,
		ProfileOptions: preparation.profileOptions,
		CACertPath:     "",
		ProxyHost:      "",
		CaptureDir:     preparation.captureDir,
		Force:          req.GetForce(),
		Log:            s.log,
		ExtraArgs:      preparation.extraArgs,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "launch MITM upstream: %v", err)
	}
	return &clydev1.LaunchMITMUpstreamResponse{
		Upstream:    preparation.upstream,
		ProfileMode: preparation.profileMode,
		CaptureDir:  preparation.effectiveCaptureDir,
		Launched:    true,
	}, nil
}

// PrepareMITMLaunch asks the daemon to prepare a MITM launch plan without
// starting the upstream process.
func (s *Server) PrepareMITMLaunch(ctx context.Context, req *clydev1.PrepareMITMLaunchRequest) (*clydev1.PrepareMITMLaunchResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	preparation, err := prepareMITMLaunchOptions(
		req.GetUpstream(),
		req.GetProfileMode(),
		req.GetCaptureDir(),
		req.GetForce(),
		req.GetArgs(),
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	peerAddr := ""
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		peerAddr = p.Addr.String()
	}
	s.log.InfoContext(ctx, "daemon.mitm.prepare_launch",
		"component", "daemon",
		"upstream", preparation.upstream,
		"profile_mode", preparation.profileMode,
		"capture_dir", preparation.effectiveCaptureDir,
		"peer_addr", peerAddr,
		"force", req.GetForce(),
		"extra_arg_count", len(req.GetArgs()),
	)
	prepared, err := prepareMITMLaunch(ctx, mitm.PrepareLaunchOptions{
		Profile:        preparation.profile,
		ProfileOptions: preparation.profileOptions,
		CACertPath:     "",
		ProxyHost:      "",
		CaptureDir:     preparation.captureDir,
		Force:          req.GetForce(),
		Log:            s.log,
		ExtraArgs:      preparation.extraArgs,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "prepare MITM launch: %v", err)
	}
	return &clydev1.PrepareMITMLaunchResponse{
		Upstream:    preparation.upstream,
		ProfileMode: preparation.profileMode,
		CaptureDir:  prepared.CaptureDir,
		Binary:      prepared.Binary,
		Args:        append([]string{}, prepared.Args...),
		Env:         append([]string{}, prepared.Env...),
	}, nil
}

type mitmLaunchPreparation struct {
	upstream            string
	profileMode         string
	captureDir          string
	effectiveCaptureDir string
	extraArgs           []string
	profile             mitm.LaunchProfile
	profileOptions      mitm.LaunchProfileOptions
}

func prepareMITMLaunchOptions(upstreamRaw, profileModeRaw, captureDirRaw string, force bool, args []string) (mitmLaunchPreparation, error) {
	upstream := strings.TrimSpace(upstreamRaw)
	if upstream == "" {
		return mitmLaunchPreparation{}, fmt.Errorf("upstream is required")
	}
	profile, err := mitm.LookupLaunchProfile(upstream)
	if err != nil {
		slog.Warn("daemon.mitm.lookup_profile_failed", "upstream", upstream, "err", err)
		return mitmLaunchPreparation{}, fmt.Errorf("lookup MITM launch profile %q: %w", upstream, err)
	}
	profileMode, err := normalizeMITMProfileMode(profileModeRaw)
	if err != nil {
		return mitmLaunchPreparation{}, err
	}
	captureDir := strings.TrimSpace(captureDirRaw)
	effectiveCaptureDir := effectiveMITMCaptureDir(captureDir)
	profileOptions := mitm.LaunchProfileOptions{
		Isolated:           false,
		IsolatedProfileDir: "",
		Force:              force,
	}
	if profileMode == mitmProfileModeIsolated {
		profileOptions.Isolated = true
		profileOptions.IsolatedProfileDir = isolatedMITMProfileDir(effectiveCaptureDir, upstream)
	}
	return mitmLaunchPreparation{
		upstream:            upstream,
		profileMode:         profileMode,
		captureDir:          captureDir,
		effectiveCaptureDir: effectiveCaptureDir,
		extraArgs:           append([]string{}, args...),
		profile:             profile,
		profileOptions:      profileOptions,
	}, nil
}

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
		env, err = mitm.ClaudeEnv(ctx, cfg.MITM, s.log)
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

func normalizeMITMProfileMode(raw string) (string, error) {
	mode := strings.TrimSpace(raw)
	if mode == "" {
		return mitmProfileModeNormal, nil
	}
	switch mode {
	case mitmProfileModeNormal, mitmProfileModeIsolated:
		return mode, nil
	default:
		return "", fmt.Errorf("profile_mode must be %q or %q", mitmProfileModeNormal, mitmProfileModeIsolated)
	}
}

func effectiveMITMCaptureDir(override string) string {
	if override != "" {
		return override
	}
	cfg, err := config.LoadGlobalOrDefault()
	if err == nil && strings.TrimSpace(cfg.MITM.CaptureDir) != "" {
		return strings.TrimSpace(cfg.MITM.CaptureDir)
	}
	return filepath.Join(config.DefaultStateDir(), "mitm")
}

func isolatedMITMProfileDir(captureDir, upstream string) string {
	if strings.TrimSpace(captureDir) == "" {
		captureDir = filepath.Join(config.DefaultStateDir(), "mitm")
	}
	return filepath.Join(captureDir, "profiles", upstream, mitmProfileModeIsolated)
}
