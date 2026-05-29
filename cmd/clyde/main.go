// Command clyde owns Clyde-specific commands for transcript search, export,
// daemon, MITM, logs, and MCP.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	cliconversation "goodkind.io/clyde/internal/cli/conversation"
	"goodkind.io/clyde/internal/cli/daemon"
	"goodkind.io/clyde/internal/cli/logs"
	"goodkind.io/clyde/internal/cli/mcp"
	cliMITM "goodkind.io/clyde/internal/cli/mitm"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logpolicy"
	_ "goodkind.io/clyde/internal/providers/claude/mitmcontrib"
	_ "goodkind.io/clyde/internal/providers/codex/mitmcontrib"
	_ "goodkind.io/clyde/internal/providers/cursor/mitmcontrib"
	"goodkind.io/clyde/internal/slogger"
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
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "config load failed:", err)
		return 1
	}
	setupPolicy, err := logpolicy.ResolveSloggerSetup(*cfg, detectSlogRole(os.Args[1:]))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "logging policy failed:", err)
		return 1
	}
	closer, err := slogger.SetupWithPolicy(setupPolicy)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "slogger setup failed:", err)
		return 1
	}
	defer func() { _ = closer.Close() }()

	slog.Info("cli.main.start", "concern", "cmd.dispatch", "component", "cli")
	f := cli.NewSystemFactory(cli.BuildInfo{Version: "DEVELOPMENT", Commit: "", Date: ""})
	root := newRoot(f)
	root.SetArgs(os.Args[1:])
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(f.IOStreams.Err, "Error:", err)
		return 1
	}
	return 0
}

func newRoot(f *cli.Factory) *cobra.Command {
	root := &cobra.Command{
		Use:     "clyde",
		Short:   "Search, inspect, and export Claude and Codex transcripts",
		Version: "DEVELOPMENT",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SilenceErrors = true
	root.SilenceUsage = true
	root.SetIn(f.IOStreams.In)
	root.SetOut(f.IOStreams.Out)
	root.SetErr(f.IOStreams.Err)

	cli.RegisterGlobalFlags(root)
	output.PersistentFlag(root)
	for _, command := range cliconversation.NewCommands(f) {
		root.AddCommand(command)
	}
	root.AddCommand(daemon.NewCmd(f))
	root.AddCommand(logs.NewCmd(f))
	root.AddCommand(cliMITM.NewCmd(f))
	root.AddCommand(mcp.NewCmd(f))
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
