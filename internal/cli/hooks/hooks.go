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
		Example: "clyde hooks run reorient after-compact",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newRunCmd(f, reorient))
	return cmd
}

func newRunCmd(f *cli.Factory, reorient hookspec.ReorientFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "run",
		Short:   "Run one installed Clyde hook.",
		Long:    "Run one installed Clyde hook, reading the client hook JSON from stdin.",
		Example: "clyde hooks run reorient after-compact",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newRunReorientCmd(f, reorient))
	return cmd
}

func newRunReorientCmd(f *cli.Factory, reorient hookspec.ReorientFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "reorient",
		Short:   "Run one Clyde reorient hook.",
		Long:    "Run one Clyde reorient hook, reading the client hook JSON from stdin.",
		Example: "clyde hooks run reorient before-compact",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newRunHookActionCmd(f, reorient, "before-compact", hookspec.HookIDReorientBeforeCompact))
	cmd.AddCommand(newRunHookActionCmd(f, reorient, "after-compact", hookspec.HookIDReorientAfterCompact))
	cmd.AddCommand(newRunHookActionCmd(f, reorient, "stop-followup", hookspec.HookIDReorientStopFollowup))
	return cmd
}

func newRunHookActionCmd(
	f *cli.Factory,
	reorient hookspec.ReorientFunc,
	action string,
	hookID hookspec.HookID,
) *cobra.Command {
	return &cobra.Command{
		Use:     action,
		Short:   "Run one Clyde reorient hook action.",
		Long:    "Run one Clyde reorient hook action, reading the client hook JSON from stdin.",
		Example: "clyde hooks run reorient " + action,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runner := hookspec.Runner{
				Registry:      hookspec.NewRegistry(),
				Input:         f.IOStreams.In,
				Output:        f.IOStreams.Out,
				Getenv:        nil,
				Reorient:      reorient,
				SnapshotStore: nil,
			}
			return runner.Run(cmd.Context(), hookID)
		},
	}
}
