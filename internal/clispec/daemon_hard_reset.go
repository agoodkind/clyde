package clispec

import (
	"context"
	"fmt"

	"goodkind.io/clyde/internal/daemon"
)

type daemonHardResetInput struct{ Apply bool }

func (daemonHardResetInput) isClispecInput() {}

type daemonHardResetPayload struct{ Apply bool }

func (daemonHardResetPayload) isClispecPrepared() {}

func daemonHardResetOp() Operation[daemonHardResetInput, daemonHardResetPayload] {
	return Operation[daemonHardResetInput, daemonHardResetPayload]{
		Name:           Name{Canonical: "daemon_hard_reset", CLIOverride: "hard-reset"},
		Group:          daemonGroup,
		Surfaces:       SurfaceSet{CLI: true, MCP: false},
		outputKind:     0,
		Args:           nil,
		Children:       nil,
		runResult:      nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Short:          "Stop Clyde, delete local database contents, and reinstall its service",
		Long:           "Delete Clyde's local databases and derived index state, preserving configuration, credentials, logs, and provider data. Reinstall the current executable through the native service manager. LMS data is preserved. Pass --apply to perform the deletion.",
		Examples:       []string{"clyde daemon hard-reset --apply"},
		Params: []Param[daemonHardResetInput]{
			BoolParam("apply", "Delete the selected Clyde data and reinstall the native service.", false,
				func(in *daemonHardResetInput, value bool) { in.Apply = value }),
		},
		New:            func() daemonHardResetInput { return daemonHardResetInput{Apply: false} },
		Prepare:        func(in daemonHardResetInput) (daemonHardResetPayload, error) { return daemonHardResetPayload(in), nil },
		Run: func(ctx context.Context, payload daemonHardResetPayload, _ Surface, sink ResultSink) error {
			output, ok := sink.(*CLISink)
			if !ok || !payload.Apply {
				return fmt.Errorf("daemon hard-reset requires --apply")
			}
			if payload.Scope == daemon.HardResetScopeAll {
				return daemon.HardReset(ctx, output.out)
			}
			return daemon.HardReset(ctx, output.out)
		},
	}
}
