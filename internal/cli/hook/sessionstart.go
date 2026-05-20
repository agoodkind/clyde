package hook

import (
	"context"
	"log/slog"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	hookpkg "goodkind.io/clyde/internal/hook"
)

func newSessionStartCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "sessionstart",
		Short: "Unified SessionStart hook handler",
		Long: `Called by Claude Code's SessionStart hook for all sources (startup, resume, compact, clear).
Handles fork registration, session ID updates, and context injection.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			log := slog.Default().With("component", "hook")
			log.Info("cli.hook.sessionstart.invoked")

			store, err := f.Store()
			if err != nil {
				return err
			}

			_, err = hookpkg.ProcessSessionStart(
				ctx,
				store,
				hookpkg.SessionStartConfig{LogRawEvent: nil},
				log,
				f.IOStreams.In,
				f.IOStreams.Out,
				f.IOStreams.Err,
			)
			if err != nil {
				return err
			}

			log.Info("cli.hook.sessionstart.completed")
			return nil
		},
	}
}
