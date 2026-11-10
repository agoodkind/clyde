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
	return newCmdWithDeps(f, daemon.ReorientConversationForHook, nil, nil)
}

func newCmdWithDeps(
	f *cli.Factory,
	reorient hookspec.ReorientFunc,
	snapshotStore hookspec.SnapshotStore,
	getenv func(string) string,
) *cobra.Command {
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
	cmd.AddCommand(newRunCmd(f, reorient, snapshotStore, getenv))
	return cmd
}

func newRunCmd(
	f *cli.Factory,
	reorient hookspec.ReorientFunc,
	snapshotStore hookspec.SnapshotStore,
	getenv func(string) string,
) *cobra.Command {
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
	cmd.AddCommand(newRunReorientCmd(f, reorient, snapshotStore, getenv))
	return cmd
}

func newRunReorientCmd(
	f *cli.Factory,
	reorient hookspec.ReorientFunc,
	snapshotStore hookspec.SnapshotStore,
	getenv func(string) string,
) *cobra.Command {
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
	cmd.AddCommand(newRunHookActionCmd(f, reorient, snapshotStore, getenv, "before-compact", hookspec.HookIDReorientBeforeCompact))
	cmd.AddCommand(newRunHookActionCmd(f, reorient, snapshotStore, getenv, "after-compact", hookspec.HookIDReorientAfterCompact))
	cmd.AddCommand(newRunHookActionCmd(f, reorient, snapshotStore, getenv, "stop-followup", hookspec.HookIDReorientStopFollowup))
	return cmd
}

func newRunHookActionCmd(
	f *cli.Factory,
	reorient hookspec.ReorientFunc,
	snapshotStore hookspec.SnapshotStore,
	getenv func(string) string,
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
				Getenv:        getenv,
				Reorient:      reorient,
				SnapshotStore: snapshotStore,
			}
			return runner.Run(cmd.Context(), hookID)
		},
	}
}
