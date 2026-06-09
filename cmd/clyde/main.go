// Command clyde owns Clyde-specific commands for transcript search, export,
// daemon, MITM, logs, and MCP.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/daemon"
	"goodkind.io/clyde/internal/cli/logs"
	"goodkind.io/clyde/internal/cli/mcp"
	cliMITM "goodkind.io/clyde/internal/cli/mitm"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/clispec"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logpolicy"
	_ "goodkind.io/clyde/internal/providers/claude/clisearch"
	_ "goodkind.io/clyde/internal/providers/claude/mitmcontrib"
	_ "goodkind.io/clyde/internal/providers/codex/mitmcontrib"
	_ "goodkind.io/clyde/internal/providers/cursor/mitmcontrib"
	"goodkind.io/clyde/internal/response"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/correlation"
)

func main() {
	slog.Debug("cli.main.entry", "concern", "cmd.dispatch", "component", "cli")
	if os.Getenv("CLYDE_SELF_RELOAD_PROBE") == "1" && len(os.Args) == 2 && os.Args[1] == "__clyde_self_reload_probe__" {
		_, _ = fmt.Fprintln(os.Stdout, "clyde-self-reload-probe:ok")
		os.Exit(0)
	}
	os.Exit(run())
}

func run() int {
	rootCtx := correlation.WithContext(context.Background(), correlation.New(""))
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		_ = response.WriteText(rootCtx, os.Stderr, "config load failed: "+err.Error()+"\n")
		return 1
	}
	setupPolicy, err := logpolicy.ResolveSloggerSetup(*cfg, detectSlogRole(os.Args[1:]))
	if err != nil {
		_ = response.WriteText(rootCtx, os.Stderr, "logging policy failed: "+err.Error()+"\n")
		return 1
	}
	closer, err := slogger.SetupWithPolicy(setupPolicy)
	if err != nil {
		_ = response.WriteText(rootCtx, os.Stderr, "slogger setup failed: "+err.Error()+"\n")
		return 1
	}
	defer func() { _ = closer.Close() }()

	slog.Info("cli.main.start", "concern", "cmd.dispatch", "component", "cli")
	f := cli.NewSystemFactory(cli.BuildInfo{Version: "DEVELOPMENT", Commit: "", Date: ""})
	root := newRoot(f)
	root.SetContext(rootCtx)
	root.SetArgs(os.Args[1:])
	if err := root.Execute(); err != nil {
		_ = response.WriteText(rootCtx, f.IOStreams.Err, "Error: "+err.Error()+"\n")
		return 1
	}
	return 0
}

func newRoot(f *cli.Factory) *cobra.Command {
	root := &cobra.Command{
		Use:     "clyde",
		Short:   "Search, inspect, and export Claude and Codex transcripts",
		Long:    "Clyde reads raw Claude and Codex conversation artifacts and exposes them through terminal commands and an MCP server, alongside the background daemon, the MITM capture proxy, log inspection, and transcript export.",
		Example: "clyde conversation list\nclyde conversation export claude:1a2b3c",
		Version: "DEVELOPMENT",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SilenceErrors = true
	root.SetIn(f.IOStreams.In)
	root.SetOut(f.IOStreams.Out)
	root.SetErr(f.IOStreams.Err)

	cli.RegisterGlobalFlags(root)
	output.PersistentFlag(root)

	reg := clispec.NewConversationRegistry()
	reg.AddHandwritten(clispec.HandwrittenCommand{Build: daemon.NewCmd})
	reg.AddHandwritten(clispec.HandwrittenCommand{Build: logs.NewCmd})
	reg.AddHandwritten(clispec.HandwrittenCommand{Build: cliMITM.NewCmd})
	reg.AddHandwritten(clispec.HandwrittenCommand{Build: mcp.NewCmd})
	for _, command := range clispec.RenderCobra(reg, f) {
		root.AddCommand(command)
	}

	cli.InstallHelpRendering(root)
	return root
}

func detectSlogRole(args []string) slogger.ProcessRole {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if arg == "daemon" {
			return slogger.ProcessRoleDaemon
		}
		return slogger.ProcessRoleCLI
	}
	return slogger.ProcessRoleCLI
}
