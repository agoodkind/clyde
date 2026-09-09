package clispec

import (
	"context"
	"fmt"

	"goodkind.io/clyde/internal/daemon"
)

type daemonHardResetInput struct{ DeleteLocalData bool }

func (daemonHardResetInput) isClispecInput() {}

type daemonHardResetPayload struct{ DeleteLocalData bool }

func (daemonHardResetPayload) isClispecPrepared() {}

func daemonHardResetOp() Operation[daemonHardResetInput, daemonHardResetPayload] {
	return Operation[daemonHardResetInput, daemonHardResetPayload]{
		Name:           Name{Canonical: "daemon_hard_reset", CLIOverride: "hard-reset"},
		Group:          daemonGroup,
		Surfaces:       SurfaceSet{CLI: true, MCP: false},
		outputKind:     0,
		Args:           nil,
		Params:         nil,
		Children:       nil,
		runResult:      nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Short:          "Stop Clyde, delete local database contents, and reinstall its service",
		Long:           "Delete Clyde's local databases and derived index state, preserving configuration, credentials, logs, and provider data. Reinstall the current executable through the native service manager. LMS data is preserved.",
		Examples:       []string{"clyde daemon hard-reset"},
		New:            func() daemonHardResetInput { return daemonHardResetInput{DeleteLocalData: true} },
		Prepare:        func(in daemonHardResetInput) (daemonHardResetPayload, error) { return daemonHardResetPayload(in), nil },
		Run: func(ctx context.Context, payload daemonHardResetPayload, _ Surface, sink ResultSink) error {
			output, ok := sink.(*CLISink)
			if !ok || !payload.DeleteLocalData {
				return fmt.Errorf("daemon hard-reset requires the CLI operator action")
			}
			return daemon.HardReset(ctx, output.out)
		},
	}
}
