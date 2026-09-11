package clispec

import (
	"context"
	"fmt"

	"goodkind.io/clyde/internal/daemon"
)

type daemonUninstallInput struct {
	Apply bool
}

func (daemonUninstallInput) isClispecInput() {}

type daemonUninstallPayload struct {
	Apply bool
}

func (daemonUninstallPayload) isClispecPrepared() {}

func daemonUninstallOp() Operation[daemonUninstallInput, daemonUninstallPayload] {
	return Operation[daemonUninstallInput, daemonUninstallPayload]{
		Name:       Name{Canonical: "daemon_uninstall", CLIOverride: "uninstall"},
		Group:      daemonGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: 0,
		Short:      "Stop and unregister the Clyde daemon",
		Long:       "Stop Clyde daemon processes and remove the native service registration. Preserve Clyde data, configuration, hooks, MCP configuration, and binaries. Pass --apply to perform the uninstall.",
		Examples:   []string{"clyde daemon uninstall --apply"},
		Args:       nil,
		Params: []Param[daemonUninstallInput]{
			BoolParam("apply", "Stop the daemon and remove its native service registration.", false,
				func(in *daemonUninstallInput, value bool) { in.Apply = value }),
		},
		New: func() daemonUninstallInput {
			return daemonUninstallInput{Apply: false}
		},
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare: func(in daemonUninstallInput) (daemonUninstallPayload, error) {
			if !in.Apply {
				return daemonUninstallPayload{}, fmt.Errorf("daemon uninstall requires --apply")
			}
			return daemonUninstallPayload(in), nil
		},
		Run: func(ctx context.Context, payload daemonUninstallPayload, _ Surface, sink ResultSink) error {
			output, ok := sink.(*CLISink)
			if !ok {
				return fmt.Errorf("daemon uninstall requires terminal output")
			}
			return daemon.Uninstall(ctx, output.out)
		},
		runResult: nil,
	}
}
