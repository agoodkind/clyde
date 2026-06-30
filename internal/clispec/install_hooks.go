package clispec

import (
	"context"
	"fmt"
	"strings"

	"goodkind.io/clyde/internal/hookspec"
)

var installGroup = &Group{
	Use:     "install",
	Short:   "Install user-scoped Clyde integrations",
	Long:    "Install user-scoped Clyde integrations such as Claude Code hooks.",
	Example: "clyde install hooks",
	Parent:  nil,
}

type installHooksInput struct {
	Client   string
	ClydeBin string
	DryRun   bool
}

func (installHooksInput) isClispecInput() {}

type installHooksPayload struct {
	Client   hookspec.Client
	ClydeBin string
	DryRun   bool
}

func (installHooksPayload) isClispecPrepared() {}

func installHooksOp() Operation[installHooksInput, installHooksPayload] {
	return Operation[installHooksInput, installHooksPayload]{
		Name:       Name{Canonical: "install_hooks", CLIOverride: "hooks"},
		Group:      installGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: resultKindValue,
		Short:      "Install user-scoped Clyde hooks.",
		Long:       "Install user-scoped Clyde hooks. By default this installs Claude Code, Codex, and Cursor hooks into user settings so they follow the user across projects.",
		Examples: []string{
			"clyde install hooks",
			"clyde install hooks --client cursor",
			"clyde install hooks --clyde-bin /usr/local/bin/clyde",
			"clyde install hooks --dry-run",
		},
		Args: nil,
		Params: []Param[installHooksInput]{
			EnumParam("client", "Hook client to install.", string(hookspec.ClientAll), []string{string(hookspec.ClientAll), string(hookspec.ClientClaudeCode), string(hookspec.ClientCodex), string(hookspec.ClientCursor)}, func(in *installHooksInput, v string) { in.Client = v }),
			StringParam("clyde_bin", "Absolute path to the clyde binary used by installed hooks.", "", false, func(in *installHooksInput, v string) { in.ClydeBin = v }),
			BoolParam("dry_run", "Print the planned settings file without writing it.", false, func(in *installHooksInput, v bool) { in.DryRun = v }),
		},
		New: func() installHooksInput {
			return installHooksInput{
				Client:   string(hookspec.ClientAll),
				ClydeBin: "",
				DryRun:   false,
			}
		},
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Children:       nil,
		Prepare:        prepareInstallHooks,
		Run:            nil,
		runResult: func(ctx context.Context, payload installHooksPayload) (Result, error) {
			installer := hookspec.Installer{Registry: hookspec.NewRegistry()}
			result, err := installer.Install(ctx, hookspec.InstallOptions{
				HomeDir:  "",
				ClydeBin: payload.ClydeBin,
				Client:   payload.Client,
				DryRun:   payload.DryRun,
			})
			if err != nil {
				return nil, logFail(ctx, surfaceFromContext(ctx), "install_hooks_failed", "install hooks", err)
			}
			output := installHooksOutputFromResult(result)
			return valueResult{
				Payload: output,
				Text:    renderInstallHooksText(result),
			}, nil
		},
	}
}

func prepareInstallHooks(input installHooksInput) (installHooksPayload, error) {
	client := hookspec.Client(strings.TrimSpace(input.Client))
	if client == "" {
		client = hookspec.ClientAll
	}
	switch client {
	case hookspec.ClientAll, hookspec.ClientClaudeCode, hookspec.ClientCodex, hookspec.ClientCursor:
	default:
		return installHooksPayload{}, fmt.Errorf("unsupported hooks client %q", client)
	}
	return installHooksPayload{
		Client:   client,
		ClydeBin: strings.TrimSpace(input.ClydeBin),
		DryRun:   input.DryRun,
	}, nil
}

func renderInstallHooksText(result hookspec.InstallResult) string {
	var builder strings.Builder
	switch {
	case result.DryRun:
		builder.WriteString("planned hooks install\n")
	case result.Changed:
		builder.WriteString("installed hooks\n")
	default:
		builder.WriteString("installed hooks already up to date\n")
	}
	for _, file := range result.Files {
		fmt.Fprintf(&builder, "settings: %s\n", file.SettingsPath)
		fmt.Fprintf(&builder, "hooks: %s\n", joinHookIDs(file.Installed))
	}
	if result.DryRun {
		for _, file := range result.Files {
			fmt.Fprintf(&builder, "\n# %s\n", file.SettingsPath)
			builder.Write(file.Preview)
		}
	}
	return builder.String()
}

func joinHookIDs(ids []hookspec.HookID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, string(id))
	}
	return strings.Join(parts, ",")
}
