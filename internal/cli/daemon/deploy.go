package daemon

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/deploy"
)

func newDeployCmd(f *cli.Factory) *cobra.Command {
	var reloadOnly bool
	cmd := &cobra.Command{
		Use:     "deploy",
		Short:   "Install or refresh the daemon service and reload it safely",
		Long:    "Deploy ensures the daemon service config matches the current binary, installs or refreshes it when needed, and otherwise performs the supervisor-aware reload or restart decision.",
		Example: "clyde daemon deploy",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := deploy.RunFromEnv(cmd.Context(), os.LookupEnv, reloadOnly, f.IOStreams.Out, f.IOStreams.Err); err != nil {
				slog.WarnContext(cmd.Context(), "cli.daemon.deploy.failed", "concern", "cli.daemon", "component", "cli", "err", err)
				return fmt.Errorf("daemon deploy: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&reloadOnly, "reload-only", false, "refuse service config changes and only perform the runtime reload-or-restart decision")
	return cmd
}
