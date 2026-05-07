package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/spf13/cobra"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	mitmcore "goodkind.io/clyde/internal/mitm"
)

// CursorProfileMode describes which Cursor profile the launcher should use.
type CursorProfileMode string

const (
	// CursorProfileDefault leaves Cursor profile selection to the launcher.
	CursorProfileDefault CursorProfileMode = ""
	// CursorProfileNormal requests the user's normal Cursor profile.
	CursorProfileNormal CursorProfileMode = "normal"
	// CursorProfileIsolated requests an isolated Cursor profile.
	CursorProfileIsolated CursorProfileMode = "isolated"
)

// LaunchRequest is the CLI-owned primitive passed to the launch boundary.
type LaunchRequest struct {
	ProfileName       string
	CaptureDir        string
	Force             bool
	CursorProfileMode CursorProfileMode
	ExtraArgs         []string
}

// UpstreamLauncher is the boundary implemented by the daemon-backed launch path.
type UpstreamLauncher interface {
	LaunchMITMUpstream(ctx context.Context, request LaunchRequest) (LaunchResponse, error)
}

// LaunchResponse is the daemon launch result the CLI prints.
type LaunchResponse struct {
	Upstream    string
	ProfileMode string
	CaptureDir  string
	Launched    bool
}

// CommandOptions configures command construction.
type CommandOptions struct {
	Launcher     UpstreamLauncher
	ProfileNames func() []string
}

// NewCmd builds the mitm command tree.
func NewCmd(f *cli.Factory) *cobra.Command {
	return NewCmdWithOptions(f, CommandOptions{
		Launcher:     nil,
		ProfileNames: nil,
	})
}

// NewCmdWithOptions builds the mitm command tree with injectable launch plumbing.
func NewCmdWithOptions(f *cli.Factory, options CommandOptions) *cobra.Command {
	if options.ProfileNames == nil {
		options.ProfileNames = launchProfileNames
	}
	if options.Launcher == nil {
		options.Launcher = daemonLauncher{}
	}

	cmd := &cobra.Command{
		Use:   "mitm",
		Short: "Manage MITM capture launchers",
	}
	cmd.AddCommand(newLaunchCmd(f, options))
	return cmd
}

func newLaunchCmd(f *cli.Factory, options CommandOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "launch",
		Short: "Launch upstream clients through the MITM proxy",
	}
	for _, profileName := range options.ProfileNames() {
		cmd.AddCommand(newProfileLaunchCmd(f, options.Launcher, profileName))
	}
	return cmd
}

func newProfileLaunchCmd(f *cli.Factory, launcher UpstreamLauncher, profileName string) *cobra.Command {
	state := launchFlagState{
		captureDir:      "",
		force:           false,
		normalProfile:   false,
		isolatedProfile: false,
	}
	cmd := &cobra.Command{
		Use:   profileName + " [-- args...]",
		Short: fmt.Sprintf("Launch %s through the MITM proxy", profileName),
		Args:  cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{
			UnknownFlags: true,
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := buildLaunchRequest(profileName, state, args)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
			defer cancel()
			if f != nil && f.Logger != nil {
				f.Logger.InfoContext(ctx,
					"cli.mitm.launch.invoked",
					"component", "cli",
					"profile", request.ProfileName,
					"force", request.Force,
					"capture_dir_set", request.CaptureDir != "",
					"extra_args_count", len(request.ExtraArgs),
				)
			}
			resp, err := launcher.LaunchMITMUpstream(ctx, request)
			if err != nil {
				slog.WarnContext(ctx, "cli.mitm.launch_failed", "upstream", request.ProfileName, "err", err)
				return fmt.Errorf("launch MITM upstream: %w", err)
			}
			if f != nil && f.IOStreams != nil && f.IOStreams.Out != nil {
				_, _ = fmt.Fprintf(f.IOStreams.Out, "launched %s via MITM (%s profile), capture_dir=%s\n",
					resp.Upstream,
					resp.ProfileMode,
					resp.CaptureDir,
				)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&state.captureDir, "capture-dir", "", "MITM capture directory override")
	cmd.Flags().BoolVar(&state.force, "force", false, "Force launch when the launcher would otherwise refuse")
	if profileName == "cursor" {
		cmd.Flags().BoolVar(&state.normalProfile, "normal-profile", false, "Launch Cursor with the normal profile")
		cmd.Flags().BoolVar(&state.isolatedProfile, "isolated-profile", false, "Launch Cursor with an isolated profile")
	}
	return cmd
}

type launchFlagState struct {
	captureDir      string
	force           bool
	normalProfile   bool
	isolatedProfile bool
}

func buildLaunchRequest(profileName string, state launchFlagState, args []string) (LaunchRequest, error) {
	cursorProfileMode, err := cursorProfileMode(profileName, state)
	if err != nil {
		return LaunchRequest{}, err
	}
	extraArgs := append([]string{}, args...)
	return LaunchRequest{
		ProfileName:       profileName,
		CaptureDir:        state.captureDir,
		Force:             state.force,
		CursorProfileMode: cursorProfileMode,
		ExtraArgs:         extraArgs,
	}, nil
}

func cursorProfileMode(profileName string, state launchFlagState) (CursorProfileMode, error) {
	if profileName != "cursor" {
		return CursorProfileDefault, nil
	}
	if state.normalProfile && state.isolatedProfile {
		return CursorProfileDefault, fmt.Errorf("--normal-profile and --isolated-profile are mutually exclusive")
	}
	if state.normalProfile {
		return CursorProfileNormal, nil
	}
	if state.isolatedProfile {
		return CursorProfileIsolated, nil
	}
	return CursorProfileDefault, nil
}

func launchProfileNames() []string {
	profiles := mitmcore.LaunchProfiles()
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type daemonLauncher struct{}

func (daemonLauncher) LaunchMITMUpstream(ctx context.Context, request LaunchRequest) (LaunchResponse, error) {
	resp, err := daemonsvc.LaunchMITMUpstreamViaDaemon(ctx, &clydev1.LaunchMITMUpstreamRequest{
		Upstream:    request.ProfileName,
		ProfileMode: string(request.CursorProfileMode),
		CaptureDir:  request.CaptureDir,
		Force:       request.Force,
		Args:        append([]string{}, request.ExtraArgs...),
	})
	if err != nil {
		slog.WarnContext(ctx, "cli.mitm.launch_daemon_rpc_failed", "upstream", request.ProfileName, "err", err)
		return LaunchResponse{}, fmt.Errorf("launch mitm upstream via daemon: %w", err)
	}
	return LaunchResponse{
		Upstream:    resp.GetUpstream(),
		ProfileMode: resp.GetProfileMode(),
		CaptureDir:  resp.GetCaptureDir(),
		Launched:    resp.GetLaunched(),
	}, nil
}
