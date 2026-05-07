package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
)

const (
	mitmProfileModeNormal   = "normal"
	mitmProfileModeIsolated = "isolated"
)

// LaunchMITMUpstream asks the daemon to start a known upstream through
// Clyde's local MITM capture proxy.
func (s *Server) LaunchMITMUpstream(ctx context.Context, req *clydev1.LaunchMITMUpstreamRequest) (*clydev1.LaunchMITMUpstreamResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	upstream := strings.TrimSpace(req.GetUpstream())
	if upstream == "" {
		return nil, status.Error(codes.InvalidArgument, "upstream is required")
	}
	profile, err := mitm.LookupLaunchProfile(upstream)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	profileMode, err := normalizeMITMProfileMode(req.GetProfileMode())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	captureDir := strings.TrimSpace(req.GetCaptureDir())
	effectiveCaptureDir := effectiveMITMCaptureDir(captureDir)
	extraArgs := append([]string{}, req.GetArgs()...)
	profileOptions := mitm.LaunchProfileOptions{Force: req.GetForce()}
	if profileMode == mitmProfileModeIsolated {
		profileOptions.Isolated = true
		profileOptions.IsolatedProfileDir = isolatedMITMProfileDir(effectiveCaptureDir, upstream)
	}
	s.log.InfoContext(ctx, "daemon.mitm.launch_upstream",
		"component", "daemon",
		"upstream", upstream,
		"profile_mode", profileMode,
		"capture_dir", effectiveCaptureDir,
		"force", req.GetForce(),
		"extra_arg_count", len(req.GetArgs()),
	)
	if err := mitm.LaunchUpstream(ctx, mitm.LaunchUpstreamOptions{
		Profile:        profile,
		ProfileOptions: profileOptions,
		CaptureDir:     captureDir,
		Force:          req.GetForce(),
		Log:            s.log,
		ExtraArgs:      extraArgs,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "launch MITM upstream: %v", err)
	}
	return &clydev1.LaunchMITMUpstreamResponse{
		Upstream:    upstream,
		ProfileMode: profileMode,
		CaptureDir:  effectiveCaptureDir,
		Launched:    true,
	}, nil
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
