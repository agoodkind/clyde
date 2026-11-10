package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/hookspec"
	"goodkind.io/clyde/internal/mcpspec"
)

type installSetupInput struct {
	Daemon bool
	Hooks  bool
	MCP    bool
}

func (installSetupInput) isClispecInput() {}

type installSetupPayload struct {
	Daemon bool
	Hooks  bool
	MCP    bool
}

func (installSetupPayload) isClispecPrepared() {}

type installSetupDependencies struct {
	installDaemon func(context.Context, ResultSink) error
	installHooks  func(context.Context, ResultSink) error
	installMCP    func(context.Context, ResultSink) error
}

func installSetupOp() Operation[installSetupInput, installSetupPayload] {
	return installSetupOpWithDependencies(defaultInstallSetupDependencies())
}

func installSetupOpWithDependencies(dependencies installSetupDependencies) Operation[installSetupInput, installSetupPayload] {
	return Operation[installSetupInput, installSetupPayload]{
		Name:       Name{Canonical: "install_setup", CLIOverride: "setup"},
		Group:      installGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: 0,
		Short:      "Install selected Clyde components",
		Long:       "Install only the selected daemon, hooks, and MCP registrations. Select at least one component.",
		Examples: []string{
			"clyde install setup --daemon",
			"clyde install setup --hooks --mcp",
		},
		Args: nil,
		Params: []Param[installSetupInput]{
			BoolParam("daemon", "Build the initial raw conversation index and install the daemon service.", false,
				func(input *installSetupInput, value bool) { input.Daemon = value }),
			BoolParam("hooks", "Install Clyde hooks in supported clients.", false,
				func(input *installSetupInput, value bool) { input.Hooks = value }),
			BoolParam("mcp", "Register Clyde's MCP server in supported clients.", false,
				func(input *installSetupInput, value bool) { input.MCP = value }),
		},
		New:            func() installSetupInput { return installSetupInput{Daemon: false, Hooks: false, MCP: false} },
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare:        prepareInstallSetup,
		Run: func(ctx context.Context, payload installSetupPayload, _ Surface, sink ResultSink) error {
			if payload.Daemon {
				if err := dependencies.installDaemon(ctx, sink); err != nil {
					slog.WarnContext(ctx, "clispec.install_setup.daemon_failed", "err", err)
					return fmt.Errorf("install daemon: %w", err)
				}
			}
			if payload.Hooks {
				if err := dependencies.installHooks(ctx, sink); err != nil {
					slog.WarnContext(ctx, "clispec.install_setup.hooks_failed", "err", err)
					return fmt.Errorf("install hooks: %w", err)
				}
			}
			if payload.MCP {
				if err := dependencies.installMCP(ctx, sink); err != nil {
					slog.WarnContext(ctx, "clispec.install_setup.mcp_failed", "err", err)
					return fmt.Errorf("install MCP: %w", err)
				}
			}
			return nil
		},
		runResult: nil,
	}
}

func prepareInstallSetup(input installSetupInput) (installSetupPayload, error) {
	if !input.Daemon && !input.Hooks && !input.MCP {
		return installSetupPayload{}, fmt.Errorf("install setup requires at least one component flag")
	}
	return installSetupPayload(input), nil
}

func defaultInstallSetupDependencies() installSetupDependencies {
	return installSetupDependencies{
		installDaemon: func(ctx context.Context, sink ResultSink) error {
			operation := daemonDeployOp()
			return operation.Run(ctx, daemonDeployPayload{ReloadOnly: false}, SurfaceCLI, sink)
		},
		installHooks: func(ctx context.Context, sink ResultSink) error {
			installer := hookspec.Installer{Registry: hookspec.NewRegistry()}
			result, err := installer.Install(ctx, hookspec.InstallOptions{HomeDir: "", ClydeBin: "", Client: hookspec.ClientAll, DryRun: false})
			if err != nil {
				slog.WarnContext(ctx, "clispec.install_setup.hooks_failed", "err", err)
				return fmt.Errorf("install Clyde hooks: %w", err)
			}
			return sink.Text(renderInstallHooksText(result))
		},
		installMCP: func(ctx context.Context, sink ResultSink) error {
			result, err := (mcpspec.Installer{}).Install(ctx, mcpspec.InstallOptions{HomeDir: "", ClydeBin: ""})
			if err != nil {
				slog.WarnContext(ctx, "clispec.install_setup.mcp_failed", "err", err)
				return fmt.Errorf("install Clyde MCP settings: %w", err)
			}
			return sink.Text(renderInstallMCPText(result))
		},
	}
}

func renderInstallMCPText(result mcpspec.InstallResult) string {
	var builder strings.Builder
	if result.Changed {
		builder.WriteString("installed MCP settings\n")
	} else {
		builder.WriteString("installed MCP settings already up to date\n")
	}
	for _, file := range result.Files {
		fmt.Fprintf(&builder, "settings: %s\n", file.SettingsPath)
	}
	return builder.String()
}
