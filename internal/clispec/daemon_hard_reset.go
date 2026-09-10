package clispec

import (
	"context"
	"fmt"
	"strings"

	"goodkind.io/clyde/internal/daemon"
)

type daemonHardResetInput struct {
	Target string
	Apply  bool
}

func (daemonHardResetInput) isClispecInput() {}

type daemonHardResetPayload struct {
	Scope daemon.HardResetScope
	Apply bool
}

func (daemonHardResetPayload) isClispecPrepared() {}

func daemonHardResetOp() Operation[daemonHardResetInput, daemonHardResetPayload] {
	return Operation[daemonHardResetInput, daemonHardResetPayload]{
		Name:           Name{Canonical: "daemon_hard_reset", CLIOverride: "hard-reset"},
		Group:          daemonGroup,
		Surfaces:       SurfaceSet{CLI: true, MCP: false},
		outputKind:     0,
		Children:       nil,
		runResult:      nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Short:          "Stop Clyde, delete local database contents, and reinstall its service",
		Long:           "Delete selected Clyde data and reinstall the current executable through the native service manager. Without a target, reset databases, state, and cache while preserving configuration. Use config only when the Clyde config directory must be deleted. LMS data is preserved. Pass --apply to perform the deletion.",
		Examples:       []string{"clyde daemon hard-reset --apply", "clyde daemon hard-reset cache --apply", "clyde daemon hard-reset config --apply"},
		Args: []Arg[daemonHardResetInput]{
			OptionalPositionalArg("target", "Optional Clyde data group: db, state, cache, config, or hooks.",
				func(in *daemonHardResetInput, value string) { in.Target = value }),
		},
		Params: []Param[daemonHardResetInput]{
			BoolParam("apply", "Delete the selected Clyde data and reinstall the native service.", false,
				func(in *daemonHardResetInput, value bool) { in.Apply = value }),
		},
		New: func() daemonHardResetInput { return daemonHardResetInput{Target: "", Apply: false} },
		Prepare: func(in daemonHardResetInput) (daemonHardResetPayload, error) {
			target := daemon.HardResetScope(strings.TrimSpace(in.Target))
			switch target {
			case daemon.HardResetScopeAll, daemon.HardResetScopeDB, daemon.HardResetScopeState, daemon.HardResetScopeCache, daemon.HardResetScopeConfig, daemon.HardResetScopeHooks:
				return daemonHardResetPayload{Scope: target, Apply: in.Apply}, nil
			default:
				return daemonHardResetPayload{}, fmt.Errorf("hard-reset target must be db, state, cache, config, or hooks")
			}
		},
		Run: func(ctx context.Context, payload daemonHardResetPayload, _ Surface, sink ResultSink) error {
			output, ok := sink.(*CLISink)
			if !ok || !payload.Apply {
				return fmt.Errorf("daemon hard-reset requires --apply")
			}
			if payload.Scope == daemon.HardResetScopeAll {
				return daemon.HardReset(ctx, output.out)
			}
			return daemon.HardResetWithOptions(ctx, output.out, daemon.HardResetOptions{Scope: payload.Scope})
		},
	}
}
