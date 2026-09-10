package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/sandbox"
)

// sandboxCollectionID names the search-engine collection every opted-in sandbox
// uses. It is one fixed name rather than one per run, because clyde can ask the
// engine to create a collection but has no call to drop one, so per-run names
// would accumulate with no way to remove them from here. Reusing one name means
// there is never more than a single sandbox collection however often the sandbox
// runs, and the operator can drop that one by hand whenever they like.
//
// Reuse also carries content between runs, so a second run re-embeds only what
// changed. A run that needs a cold start drops this collection first.
const sandboxCollectionID = "clyde-sandbox"

// sandboxConfigTemplate is the config a sandbox daemon boots with, with its
// semantic directions and collection id substituted.
//
// The adapter and MITM listeners stay off because they would bind ports the
// deployed daemon already owns and neither takes part in reading or searching
// conversations.
//
// Semantic ingestion and search are independent opt-ins. When either is enabled,
// it uses the same engine as the deployed daemon but a collection of the sandbox's
// own. Leaving the socket path unset resolves the engine exactly as production
// does.
const sandboxConfigTemplate = `[logging]
level = "debug"

[conversation.semantic]
ingestion_enabled = %t
search_enabled = %t
collection_id = %q

[adapter]
enabled = false

[mitm]
enabled_default = false
`

// newSandboxCmd builds the sandbox daemon command.
//
// The sandbox is a second daemon for hands-on validation. It runs the same
// binary as production with its state, config, cache, and runtime socket
// redirected into throwaway directories, so it cannot disturb the operator's
// daemon and the operator's daemon cannot disturb it.
//
// It reads the operator's real Cursor stores, because Cursor keeps its
// conversations outside the XDG directories this command redirects. The
// conversation cache does live under XDG, so a sandbox starts with no cache and
// reads every conversation from the provider stores on its first pass. That is
// what makes it useful for judging a change to how conversations are read.
func newSandboxCmd(f *cli.Factory) *cobra.Command {
	var keep bool
	var ingestionEnabled bool
	var searchEnabled bool
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Run a throwaway second daemon for hands-on validation",
		Long: "Run a second daemon with throwaway state, config, cache, and socket, " +
			"isolated from the deployed daemon. It reads the same provider conversation " +
			"stores with an empty cache, so it exercises conversation reading from scratch. " +
			"Every listener is disabled. Press Ctrl-C to stop it.",
		Example: "clyde daemon sandbox",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandbox(cmd.Context(), f, keep, ingestionEnabled, searchEnabled)
		},
	}
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the sandbox directories after exit instead of removing them")
	cmd.Flags().BoolVar(&ingestionEnabled, "ingestion-enabled", false, "offer conversations to the semantic search engine")
	cmd.Flags().BoolVar(&searchEnabled, "search-enabled", false, "answer conversation searches from the semantic search engine")
	return cmd
}

// runSandbox prepares the throwaway roots, writes the listener-free config, and
// runs the daemon in this process until the operator stops it.
//
// It runs the worker directly rather than starting `daemon run`, which would
// spawn a supervisor that spawns a worker. Those extra processes have no job
// here: a sandbox never reloads or rebinds, and a supervisor's replacement
// worker escapes the process group, so a wrapper killed without a signal leaves
// a daemon serving with nothing left to stop it. One process cannot.
func runSandbox(ctx context.Context, f *cli.Factory, keep bool, ingestionEnabled bool, searchEnabled bool) error {
	roots, err := sandbox.NewRoots()
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.sandbox.root_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
		return fmt.Errorf("prepare the sandbox directories: %w", err)
	}
	if !keep {
		defer func() { _ = os.RemoveAll(roots.Base) }()
	}
	// The roots are removed on exit, so verify they are throwaway paths before
	// anything is written into them.
	if err := sandbox.PreflightRoots(roots); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.sandbox.preflight_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
		return fmt.Errorf("check the sandbox directories: %w", err)
	}

	configPath := filepath.Join(roots.Config, "clyde", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.sandbox.config_dir_failed", "concern", "cmd.dispatch", "component", "cli", "path", configPath, "err", err)
		return fmt.Errorf("create the sandbox config directory %s: %w", filepath.Dir(configPath), err)
	}
	sandboxConfig := fmt.Sprintf(sandboxConfigTemplate, ingestionEnabled, searchEnabled, sandboxCollectionID)
	if err := os.WriteFile(configPath, []byte(sandboxConfig), 0o600); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.sandbox.config_write_failed", "concern", "cmd.dispatch", "component", "cli", "path", configPath, "err", err)
		return fmt.Errorf("write the sandbox config %s: %w", configPath, err)
	}

	writeSandboxBanner(f, roots, configPath, ingestionEnabled, searchEnabled)

	// The daemon reads these when it loads its config below, so they have to be
	// set on this process rather than handed to a child.
	if err := sandbox.Apply(roots); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.sandbox.env_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
		return fmt.Errorf("point this process at the sandbox directories: %w", err)
	}

	// A launcher that dies without signalling leaves nothing to stop the daemon,
	// so watch for that too. RunContext returns when this context is cancelled.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(ctx, "cli.daemon.sandbox.parent_watch_panic", "concern", "cmd.dispatch", "component", "cli", "err", fmt.Errorf("watching for the parent's exit panicked: %v", recovered))
			}
		}()
		sandbox.WatchParent(runCtx, stop)
	}()

	// RunContext installs its own SIGINT and SIGTERM handling and returns when
	// one arrives, so Ctrl-C stops the daemon and unwinds to the cleanup above.
	if err := daemonsvc.RunContext(runCtx, slog.Default().With("component", "daemon")); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.sandbox.daemon_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
		return fmt.Errorf("run the sandbox daemon: %w", err)
	}
	return nil
}

// writeSandboxBanner prints what the sandbox is and how to drive it, so the
// operator does not have to derive the environment from the source.
func writeSandboxBanner(f *cli.Factory, roots sandbox.Roots, configPath string, ingestionEnabled bool, searchEnabled bool) {
	out := f.IOStreams.Out
	_, _ = fmt.Fprintln(out, "sandbox daemon")
	_, _ = fmt.Fprintf(out, "  root:   %s\n", roots.Base)
	_, _ = fmt.Fprintf(out, "  config: %s\n", configPath)
	_, _ = fmt.Fprintln(out, "  reads:  the real provider conversation stores, with an empty cache")
	_, _ = fmt.Fprintf(out, "  ingestion enabled: %t\n", ingestionEnabled)
	_, _ = fmt.Fprintf(out, "  search enabled:    %t\n", searchEnabled)
	_, _ = fmt.Fprintln(out, "  binds:  nothing, every listener is disabled")
	_, _ = fmt.Fprintln(out, "  runs:   in this process, with no supervisor and no worker to outlive it")
	_, _ = fmt.Fprintln(out, "")
	_, _ = fmt.Fprintln(out, "drive it from another terminal with this prefix:")
	_, _ = fmt.Fprintf(out, "  %s %s\n", sandbox.ExportLine(roots), cli.ConversationBrowseCommand())
	_, _ = fmt.Fprintln(out, "")
	_, _ = fmt.Fprintln(out, "Ctrl-C stops it.")
	_, _ = fmt.Fprintln(out, "")
}
