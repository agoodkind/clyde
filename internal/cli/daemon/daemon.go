package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
)

func NewCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "daemon",
		Short:  "Start the background daemon (internal)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.Info("cli.daemon.invoked",
				"component", "cli",
				"version", f.Build.Version,
			)
			log := slog.Default().With("component", "daemon")
			return daemonsvc.Run(log, pruneLoop(), oauthLoop(), driftLoop())
		},
	}
	cmd.AddCommand(newReloadCmd(f))
	cmd.AddCommand(newLaunchRemoteWorkerCmd(f))
	return cmd
}

func newReloadCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Reload the running daemon without restarting it",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 12*time.Second)
			defer cancel()
			resp, err := daemonsvc.ReloadDaemon(ctx)
			if err != nil {
				return err
			}
			status := "unchanged"
			if resp.GetBinaryReloaded() {
				status = "reloaded"
			}
			_, _ = fmt.Fprintf(f.IOStreams.Out, "daemon binary %s: active_sessions=%d new_pid=%d\n", status, resp.GetActiveSessions(), resp.GetNewPid())
			return nil
		},
	}
}
