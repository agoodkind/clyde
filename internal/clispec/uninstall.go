package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/hookspec"
	"goodkind.io/clyde/internal/mcpspec"
)

type uninstallInput struct {
	Apply  bool
	Daemon bool
	Hooks  bool
	MCP    bool
	Binary bool
}

func (uninstallInput) isClispecInput() {}

type uninstallPayload struct {
	Daemon bool
	Hooks  bool
	MCP    bool
	Binary bool
}

func (uninstallPayload) isClispecPrepared() {}

type uninstallDependencies struct {
	uninstallDaemon func(context.Context, ResultSink) error
	uninstallHooks  func(context.Context, ResultSink) error
	uninstallMCP    func(context.Context, ResultSink) error
	uninstallBinary func(context.Context, ResultSink) error
}

func uninstallOp() Operation[uninstallInput, uninstallPayload] {
	return uninstallOpWithDependencies(defaultUninstallDependencies())
}

func uninstallOpWithDependencies(dependencies uninstallDependencies) Operation[uninstallInput, uninstallPayload] {
	return Operation[uninstallInput, uninstallPayload]{
		Name:       Name{Canonical: "uninstall", CLIOverride: ""},
		Group:      nil,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: 0,
		Short:      "Remove selected Clyde components",
		Long:       "Remove Clyde's daemon service, hooks, and MCP registrations by default. Preserve configuration, cache, state, logs, exports, credentials, provider data, repositories, LMS data, and the binary. Any component selector restricts removal to the selected components. Pass --apply to perform the uninstall.",
		Examples: []string{
			"clyde uninstall --apply",
			"clyde uninstall --hooks --apply",
			"clyde uninstall --binary --apply",
		},
		Args: nil,
		Params: []Param[uninstallInput]{
			BoolParam("apply", "Apply the selected removals.", false, func(input *uninstallInput, value bool) { input.Apply = value }),
			BoolParam("daemon", "Stop and unregister the Clyde daemon.", false, func(input *uninstallInput, value bool) { input.Daemon = value }),
			BoolParam("hooks", "Remove Clyde hook commands from supported clients.", false, func(input *uninstallInput, value bool) { input.Hooks = value }),
			BoolParam("mcp", "Remove Clyde MCP entries from supported clients.", false, func(input *uninstallInput, value bool) { input.MCP = value }),
			BoolParam("binary", "Remove the executable running this command.", false, func(input *uninstallInput, value bool) { input.Binary = value }),
		},
		New: func() uninstallInput {
			return uninstallInput{Apply: false, Daemon: false, Hooks: false, MCP: false, Binary: false}
		},
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare:        prepareUninstall,
		Run: func(ctx context.Context, payload uninstallPayload, _ Surface, sink ResultSink) error {
			steps := []struct {
				selected bool
				name     string
				run      func(context.Context, ResultSink) error
			}{
				{selected: payload.Daemon, name: "daemon", run: dependencies.uninstallDaemon},
				{selected: payload.Hooks, name: "hooks", run: dependencies.uninstallHooks},
				{selected: payload.MCP, name: "MCP", run: dependencies.uninstallMCP},
				{selected: payload.Binary, name: "binary", run: dependencies.uninstallBinary},
			}
			for _, step := range steps {
				if !step.selected {
					continue
				}
				if err := step.run(ctx, sink); err != nil {
					slog.WarnContext(ctx, "clispec.uninstall_component_failed", "component", step.name, "err", err)
					return fmt.Errorf("uninstall %s: %w", step.name, err)
				}
			}
			return nil
		},
		runResult: nil,
	}
}

func prepareUninstall(input uninstallInput) (uninstallPayload, error) {
	if !input.Apply {
		return uninstallPayload{}, fmt.Errorf("uninstall requires --apply")
	}
	if !input.Daemon && !input.Hooks && !input.MCP && !input.Binary {
		input.Daemon = true
		input.Hooks = true
		input.MCP = true
	}
	return uninstallPayload{Daemon: input.Daemon, Hooks: input.Hooks, MCP: input.MCP, Binary: input.Binary}, nil
}

func defaultUninstallDependencies() uninstallDependencies {
	return uninstallDependencies{
		uninstallDaemon: func(ctx context.Context, sink ResultSink) error {
			output, ok := sink.(*CLISink)
			if !ok {
				return fmt.Errorf("daemon uninstall requires terminal output")
			}
			return daemon.Uninstall(ctx, output.out)
		},
		uninstallHooks: func(ctx context.Context, sink ResultSink) error {
			result, err := (hookspec.Uninstaller{Registry: hookspec.NewRegistry()}).Uninstall(ctx, hookspec.UninstallOptions{HomeDir: ""})
			if err != nil {
				slog.WarnContext(ctx, "clispec.uninstall_hooks_failed", "err", err)
				return fmt.Errorf("remove Clyde hooks: %w", err)
			}
			if err := sink.Text(renderUninstallText("hooks", result.Changed)); err != nil {
				return fmt.Errorf("write hooks uninstall result: %w", err)
			}
			return nil
		},
		uninstallMCP: func(ctx context.Context, sink ResultSink) error {
			result, err := (mcpspec.Uninstaller{}).Uninstall(ctx, mcpspec.UninstallOptions{HomeDir: ""})
			if err != nil {
				slog.WarnContext(ctx, "clispec.uninstall_mcp_failed", "err", err)
				return fmt.Errorf("remove Clyde MCP settings: %w", err)
			}
			if err := sink.Text(renderUninstallText("MCP settings", result.Changed)); err != nil {
				return fmt.Errorf("write MCP uninstall result: %w", err)
			}
			return nil
		},
		uninstallBinary: uninstallCurrentBinary,
	}
}

func uninstallCurrentBinary(ctx context.Context, sink ResultSink) error {
	path, err := os.Executable()
	if err != nil {
		slog.WarnContext(ctx, "clispec.uninstall_binary_resolve_failed", "err", err)
		return fmt.Errorf("resolve Clyde binary: %w", err)
	}
	return uninstallBinaryAt(ctx, sink, path)
}

func uninstallBinaryAt(ctx context.Context, sink ResultSink, path string) error {
	slog.InfoContext(ctx, "clispec.uninstall_binary_started", "path", path)
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "clispec.uninstall_binary_failed", "path", path, "err", err)
		return fmt.Errorf("remove Clyde binary canceled: %w", err)
	}
	if err := os.Remove(path); err != nil {
		slog.WarnContext(ctx, "clispec.uninstall_binary_failed", "path", path, "err", err)
		return fmt.Errorf("remove Clyde binary %s: %w", path, err)
	}
	if err := sink.Text("removed binary: " + path + "\n"); err != nil {
		slog.WarnContext(ctx, "clispec.uninstall_binary_output_failed", "path", path, "err", err)
		return fmt.Errorf("write binary uninstall result: %w", err)
	}
	slog.InfoContext(ctx, "clispec.uninstall_binary_completed", "path", path)
	return nil
}

func renderUninstallText(component string, changed bool) string {
	if changed {
		return "removed " + component + "\n"
	}
	return strings.ToLower(component) + " already absent\n"
}
