package clispec

import (
	"context"
	"fmt"
	"io"
	"os"

	"goodkind.io/clyde/internal/deploy"
)

type daemonControlInput struct {
	Apply bool
}

func (daemonControlInput) isClispecInput() {}

type daemonControlPayload struct {
	Apply bool
}

func (daemonControlPayload) isClispecPrepared() {}

type daemonControlRun func(context.Context, func(string) (string, bool), io.Writer) error

func daemonStopOp() Operation[daemonControlInput, daemonControlPayload] {
	return daemonControlOp(
		Name{Canonical: "daemon_stop", CLIOverride: "stop"},
		"Stop the Clyde daemon and retain its service registration",
		"Stop the native Clyde daemon service. Preserve its registration and automatic-start setting. Pass --apply to stop it.",
		deploy.StopFromEnv,
	)
}

func daemonDisableOp() Operation[daemonControlInput, daemonControlPayload] {
	return daemonControlOp(
		Name{Canonical: "daemon_disable", CLIOverride: "disable"},
		"Stop and disable the Clyde daemon while retaining its registration",
		"Stop the native Clyde daemon service and disable automatic start. Preserve its service registration. Pass --apply to disable it.",
		deploy.DisableFromEnv,
	)
}

func daemonControlOp(name Name, short string, long string, run daemonControlRun) Operation[daemonControlInput, daemonControlPayload] {
	return Operation[daemonControlInput, daemonControlPayload]{
		Name:       name,
		Group:      daemonGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: 0,
		Short:      short,
		Long:       long,
		Examples:   []string{"clyde daemon " + name.CLI() + " --apply"},
		Args:       nil,
		Params: []Param[daemonControlInput]{
			BoolParam("apply", "Apply the daemon service change.", false,
				func(input *daemonControlInput, value bool) { input.Apply = value }),
		},
		New: func() daemonControlInput {
			return daemonControlInput{Apply: false}
		},
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare: func(input daemonControlInput) (daemonControlPayload, error) {
			if !input.Apply {
				return daemonControlPayload{}, fmt.Errorf("daemon %s requires --apply", name.CLI())
			}
			return daemonControlPayload{Apply: true}, nil
		},
		Run: func(ctx context.Context, _ daemonControlPayload, _ Surface, sink ResultSink) error {
			output, ok := sink.(*CLISink)
			if !ok {
				return fmt.Errorf("daemon %s requires terminal output", name.CLI())
			}
			return run(ctx, os.LookupEnv, output.out)
		},
		runResult: nil,
	}
}
