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
//	clyde daemon                -> long-lived daemon (adapter, oauth, mcp, prune)
//	clyde hook sessionstart     -> Claude Code SessionStart hook
//	clyde mcp                   -> MCP stdio server (in-chat search/list/context)
//	clyde resume <name|uuid>    -> resolve clyde name then claude --resume <uuid>
//	clyde -r / --resume         -> TUI (same as no args; bare flag opens dashboard)
//	clyde -r / --resume <x>     -> rewritten to `clyde resume <x>` by ClassifyArgs
//	anything else               -> unknown -> ForwardToClaudeThenDashboard (see cmd/root.go)
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/cmd"
	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/compact"
	"goodkind.io/clyde/internal/cli/daemon"
	hook "goodkind.io/clyde/internal/cli/hook"
	"goodkind.io/clyde/internal/cli/mcp"
	cliMITM "goodkind.io/clyde/internal/cli/mitm"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/providers/registry"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog"
)

func main() {
	if os.Getenv("CLYDE_SELF_RELOAD_PROBE") == "1" && len(os.Args) == 2 && os.Args[1] == "__clyde_self_reload_probe__" {
		_, _ = fmt.Fprintln(os.Stdout, "clyde-self-reload-probe:ok")
		os.Exit(0)
	}

	exitCode := run()
	clydeMainLog.Logger().Info("cli.main.exit", "component", "cli", "exit_code", exitCode)
	os.Exit(exitCode)
}

func run() int {
	registry.RegisterDefaultDiscoveryScanners()

	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "config load failed:", err)
		return 1
	}

	closer, err := slogger.Setup(cfg.Logging, detectSlogRole(os.Args[1:]), buildConcernRotationOverrides(cfg))
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
			return cmd.ForwardToClaudeThenDashboard(os.Args[1:])
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
		Short:   "Named sessions and append-only compaction for Claude Code",
		Long:    `Clyde wraps Claude Code with human-friendly session names and append-only compaction. Run with no args for the TUI dashboard.`,
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
	root.AddCommand(daemon.NewCmd(f))
	root.AddCommand(hook.NewCmd(f))
	root.AddCommand(cliMITM.NewCmd(f))
	root.AddCommand(mcp.NewCmd(f))
	root.AddCommand(cmd.NewResumeCmd())

	root.SilenceErrors = true

	if err := root.Execute(); err != nil {
		if strings.HasPrefix(err.Error(), "unknown command") {
			return cmd.ForwardToClaudeThenDashboard(os.Args[1:])
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

// buildConcernRotationOverrides translates typed adapter sub-blocks into the
// per-concern rotation map slogger.Setup consumes. Today only the wire_capture
// concerns honor an override; the empty map is a no-op so existing callers
// pre-extension stay unchanged. New concerns that need their own rotation
// budget add an entry here.
func buildConcernRotationOverrides(cfg *config.Config) slogger.ConcernRotationOverrides {
	rot := cfg.Adapter.WireCapture.Rotation
	if rot.MaxSizeMB == 0 && rot.MaxBackups == 0 && rot.MaxAgeDays == 0 && rot.Enabled == nil && rot.Compress == nil {
		return slogger.ConcernRotationOverrides{}
	}
	compress := rot.Compress
	if compress == nil {
		t := true
		compress = &t
	}
	cfgRot := wireCaptureRotation(rot, compress)
	return slogger.ConcernRotationOverrides{
		slogger.ConcernAdapterProviderAnthWire:  cfgRot,
		slogger.ConcernAdapterProviderCodexWire: cfgRot,
	}
}

// wireCaptureRotation maps the typed [adapter.wire_capture.rotation] block
// onto gklog's concrete RotationConfig. Defaults are intentionally small so
// always-on use stays bounded; operators set explicit values when they need
// more retention.
func wireCaptureRotation(rot config.LoggingRotation, compress *bool) gklog.RotationConfig {
	maxSize := rot.MaxSizeMB
	if maxSize <= 0 {
		maxSize = 8
	}
	backups := rot.MaxBackups
	if backups <= 0 {
		backups = 3
	}
	age := rot.MaxAgeDays
	if age <= 0 {
		age = 2
	}
	return gklog.RotationConfig{
		MaxSizeMB:  maxSize,
		MaxBackups: backups,
		MaxAgeDays: age,
		Compress:   compress,
	}
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
