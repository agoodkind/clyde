// Package hooks exposes Clyde hook runtime commands.
package hooks

import (
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/hookspec"
)

// NewCmd builds the hooks runtime command.
func NewCmd(f *cli.Factory) *cobra.Command {
	return NewCmdWithReorient(f, daemon.ReorientConversationForHook)
}

// NewCmdWithReorient builds the hooks runtime command with an injectable
// reorient dependency for tests.
func NewCmdWithReorient(f *cli.Factory, reorient hookspec.ReorientFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "hooks",
		Short:   "Run installed Clyde hooks",
		Long:    "Run installed Clyde hooks. Hook clients call these commands from user-scoped settings.",
		Example: "clyde hooks run reorient-after-compact",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newRunCmd(f, reorient))
	return cmd
}

func newRunCmd(f *cli.Factory, reorient hookspec.ReorientFunc) *cobra.Command {
	return &cobra.Command{
		Use:     "run HOOK_ID",
		Short:   "Run one installed Clyde hook.",
		Long:    "Run one installed Clyde hook by id, reading the client hook JSON from stdin.",
		Example: "clyde hooks run reorient-after-compact",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner := hookspec.Runner{
				Registry:      hookspec.NewRegistry(),
				Input:         f.IOStreams.In,
				Output:        f.IOStreams.Out,
				Reorient:      reorient,
				SnapshotStore: nil,
			}
			return runner.Run(cmd.Context(), hookspec.HookID(args[0]))
		},
	}
}
