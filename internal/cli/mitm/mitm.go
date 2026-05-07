// Package mitm implements clyde mitm commands.
package mitm

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
)

const (
	cursorUpstream      = "cursor"
	profileModeNormal   = "normal"
	profileModeIsolated = "isolated"
)

// NewCmd returns the parent command for daemon-owned MITM launch helpers.
func NewCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mitm",
		Short: "Launch supported clients through Clyde MITM capture",
	}
	cmd.AddCommand(newLaunchCmd(f))
	return cmd
}

func newLaunchCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "launch",
		Short: "Launch an upstream client through Clyde MITM capture",
	}
	cmd.AddCommand(newLaunchCursorCmd(f))
	return cmd
}

func newLaunchCursorCmd(f *cli.Factory) *cobra.Command {
	var normalProfile bool
	var isolatedProfile bool
	var captureDir string
	var force bool

	cmd := &cobra.Command{
		Use:                "cursor [-- args...]",
		Short:              "Launch Cursor through Clyde MITM capture",
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(cmd *cobra.Command, args []string) error {
			if isolatedProfile && cmd.Flags().Changed("normal-profile") && normalProfile {
				return fmt.Errorf("--normal-profile and --isolated-profile are mutually exclusive")
			}
			profileMode := profileModeNormal
			if isolatedProfile {
				profileMode = profileModeIsolated
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
			defer cancel()
			resp, err := daemonsvc.LaunchMITMUpstreamViaDaemon(ctx, &clydev1.LaunchMITMUpstreamRequest{
				Upstream:    cursorUpstream,
				ProfileMode: profileMode,
				CaptureDir:  captureDir,
				Force:       force,
				Args:        append([]string{}, args...),
			})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(f.IOStreams.Out, "launched %s via MITM (%s profile), capture_dir=%s\n",
				resp.GetUpstream(),
				resp.GetProfileMode(),
				resp.GetCaptureDir(),
			)
			return nil
		},
	}
	cmd.Flags().BoolVar(&normalProfile, "normal-profile", true, "Launch Cursor with the normal profile")
	cmd.Flags().BoolVar(&isolatedProfile, "isolated-profile", false, "Launch Cursor with an isolated profile under the MITM capture dir")
	cmd.Flags().StringVar(&captureDir, "capture-dir", "", "Override the MITM capture directory for this launch")
	cmd.Flags().BoolVar(&force, "force", false, "Pass force launch policy to the daemon")
	return cmd
}
