// Command clyde is the user facing entrypoint.
//
// The cobra root is assembled here because this is the only place in
// the build graph that can import both goodkind.io/clyde/internal/cli
// (for Factory + IOStreams) and the per-verb sub-packages under
// internal/cli/<verb>. Putting the assembly inside internal/cli would
// create an import cycle.
//
// Argument surface:
//
//	clyde                       -> TUI dashboard (cmd.RunDashboard)
//	clyde compact ...           -> append-only compaction
//	clyde codex ...             -> explicit Codex CLI override
//	clyde daemon                -> long-lived daemon (adapter, oauth, mcp, prune)
//	clyde hook sessionstart     -> Claude Code SessionStart hook
//	clyde mcp                   -> MCP stdio server (in-chat search/list/context)
//	clyde resume <name|uuid>    -> resolve clyde name then hand off to the owning provider
//	clyde -r / --resume         -> TUI (same as no args; bare flag opens dashboard)
//	clyde -r / --resume <x>     -> rewritten to `clyde resume <x>` by ClassifyArgs
//	anything else               -> unknown -> ForwardToDefaultProviderThenDashboard (see cmd/root.go)
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/cmd"
	"goodkind.io/clyde/internal/cli"
	codexcmd "goodkind.io/clyde/internal/cli/codex"
	"goodkind.io/clyde/internal/cli/compact"
	"goodkind.io/clyde/internal/cli/daemon"
	hook "goodkind.io/clyde/internal/cli/hook"
	"goodkind.io/clyde/internal/cli/logs"
	"goodkind.io/clyde/internal/cli/mcp"
	cliMITM "goodkind.io/clyde/internal/cli/mitm"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logpolicy"
	"goodkind.io/clyde/internal/providers/registry"
	"goodkind.io/clyde/internal/slogger"
)

func main() {
	if os.Getenv("CLYDE_SELF_RELOAD_PROBE") == "1" && len(os.Args) == 2 && os.Args[1] == "__clyde_self_reload_probe__" {
		_, _ = fmt.Fprintln(os.Stdout, "clyde-self-reload-probe:ok")
		os.Exit(0)
	}

	exitCode := run()
	if !isReadOnlyLogsInventoryCommand(os.Args[1:]) {
		clydeMainLog.Logger().Info("cli.main.exit", "component", "cli", "exit_code", exitCode)
	}
	os.Exit(exitCode)
}

func run() int {
	if isReadOnlyLogsInventoryCommand(os.Args[1:]) {
		return runReadOnlyLogsCommand(os.Args[1:])
	}

	registry.RegisterDefaultDiscoveryScanners()

	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "config load failed:", err)
		return 1
	}

	role := detectSlogRole(os.Args[1:])
	setupPolicy, err := logpolicy.ResolveSloggerSetup(*cfg, role)
	if err != nil {
		clydeMainLog.Logger().Error("clyde.logpolicy.resolve_failed",
			"component", "cli",
			"err", err,
		)
		_, _ = fmt.Fprintln(os.Stderr, "logging policy failed:", err)
		return 1
	}
	closer, err := slogger.SetupWithPolicy(setupPolicy)
	if err != nil {
		clydeMainLog.Logger().Error("clyde.slogger.setup_failed",
			"component", "cli",
			"err", err,
		)
		_, _ = fmt.Fprintln(os.Stderr, "slogger setup failed:", err)
		return 1
	}
	defer func() { _ = closer.Close() }()

	if len(os.Args) > 1 {
		mode, rewritten := cmd.ClassifyArgs(os.Args[1:])
		switch mode {
		case cmd.ModePassthrough:
			return cmd.ForwardToDefaultProviderThenDashboard(os.Args[1:])
		case cmd.ModeBasedirLaunch:
			if len(rewritten) == 0 {
				return 1
			}
			return cmd.RunBasedirLaunch(rewritten[0])
		case cmd.ModeResumeNoArgDashboard:
			os.Args = os.Args[:1]
		case cmd.ModeResumeFlag:
			os.Args = append(os.Args[:1], rewritten...)
		}
	}

	clydeMainLog.Logger().Debug("cli.execute.invoked", "component", "cli")

	dashboardExitCode := 0
	root := &cobra.Command{
		Use:     "clyde",
		Short:   "Named sessions and provider-aware tooling for local coding CLIs",
		Long:    `Clyde owns the local session dashboard and provider-aware tooling. Run with no args for the TUI dashboard; use clyde codex for the explicit Codex override.`,
		Version: "DEVELOPMENT",
		Run: func(c *cobra.Command, args []string) {
			dashboardExitCode = cmd.RunDashboard(c, args)
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true

	cli.RegisterGlobalFlags(root)

	f := cli.NewSystemFactory(cli.BuildInfo{Version: "DEVELOPMENT"})

	root.SetIn(f.IOStreams.In)
	root.SetOut(f.IOStreams.Out)
	root.SetErr(f.IOStreams.Err)

	root.AddCommand(compact.NewCmd(f))
	root.AddCommand(codexcmd.NewCmd(f))
	root.AddCommand(daemon.NewCmd(f))
	root.AddCommand(hook.NewCmd(f))
	root.AddCommand(logs.NewCmd(f))
	root.AddCommand(cliMITM.NewCmd(f))
	root.AddCommand(mcp.NewCmd(f))
	root.AddCommand(cmd.NewResumeCmd())

	root.SilenceErrors = true

	if err := root.Execute(); err != nil {
		if strings.HasPrefix(err.Error(), "unknown command") {
			return cmd.ForwardToDefaultProviderThenDashboard(os.Args[1:])
		}
		_, _ = fmt.Fprintln(f.IOStreams.Err, "Error:", err)
		return 1
	}
	if dashboardExitCode != 0 {
		return dashboardExitCode
	}
	clydeMainLog.Logger().Info("cli.execute.completed", "component", "cli")
	return 0
}

func isReadOnlyLogsInventoryCommand(args []string) bool {
	nonFlagArgs := make([]string, 0, 2)
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "--config" || arg == "--state-root" || arg == "--largest" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "--config=") ||
			strings.HasPrefix(arg, "--state-root=") ||
			strings.HasPrefix(arg, "--largest=") ||
			strings.HasPrefix(arg, "-") {
			continue
		}
		nonFlagArgs = append(nonFlagArgs, arg)
		if len(nonFlagArgs) == 2 {
			break
		}
	}
	return len(nonFlagArgs) == 2 && nonFlagArgs[0] == "logs" && nonFlagArgs[1] == "inventory"
}

func runReadOnlyLogsCommand(args []string) int {
	root := &cobra.Command{
		Use:           "clyde",
		SilenceErrors: true,
	}
	cli.RegisterGlobalFlags(root)
	f := cli.NewSystemFactory(cli.BuildInfo{Version: "DEVELOPMENT"})
	root.SetIn(f.IOStreams.In)
	root.SetOut(f.IOStreams.Out)
	root.SetErr(f.IOStreams.Err)
	root.AddCommand(logs.NewCmd(f))
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(f.IOStreams.Err, "Error:", err)
		return 1
	}
	return 0
}

func detectSlogRole(args []string) slogger.ProcessRole {
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		switch {
		case arg == "-r", arg == "--resume", arg == "--claude-bin":
			skipNext = true
			continue
		case strings.HasPrefix(arg, "--resume="), strings.HasPrefix(arg, "--claude-bin="):
			continue
		case strings.HasPrefix(arg, "-"):
			continue
		case arg == "daemon":
			return slogger.ProcessRoleDaemon
		default:
			return slogger.ProcessRoleTUI
		}
	}
	return slogger.ProcessRoleTUI
}
