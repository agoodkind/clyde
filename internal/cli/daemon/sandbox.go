package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

// sandboxRootPrefix names the temp directory holding a sandbox daemon's runtime
// socket. The socket path must fit macOS's ~104-character sun_path limit, which
// the usual long temp paths overflow, so the runtime root lives directly under
// /tmp while the other roots may sit anywhere.
const sandboxRootPrefix = "clyde-sandbox-"

// sandboxCollectionID names the search-engine collection every sandbox writes
// into. It is one fixed name rather than one per run, because clyde can ask the
// engine to create a collection but has no call to drop one, so per-run names
// would accumulate with no way to remove them from here. Reusing one name means
// there is never more than a single sandbox collection however often the sandbox
// runs, and the operator can drop that one by hand whenever they like.
//
// Reuse also carries content between runs, so a second run re-embeds only what
// changed. A run that needs a cold start drops this collection first.
const sandboxCollectionID = "clyde-sandbox"

// sandboxConfigTemplate is the config a sandbox daemon boots with, with its
// collection id substituted.
//
// The adapter and MITM listeners stay off because they would bind ports the
// deployed daemon already owns and neither takes part in reading or searching
// conversations.
//
// Semantic search is on, against the same engine the deployed daemon uses but a
// collection of the sandbox's own. That is what makes this an end-to-end target
// rather than a read check: a conversation is read, embedded, and searched back,
// so a defect anywhere along that path shows up in the search result. Leaving the
// socket path unset resolves the engine exactly as production does.
const sandboxConfigTemplate = `[logging]
level = "debug"

[conversation.semantic]
enabled = true
search_enabled = true
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
			return runSandbox(cmd.Context(), f, keep)
		},
	}
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the sandbox directories after exit instead of removing them")
	return cmd
}

// sandboxRoots holds the throwaway directories one sandbox daemon runs against.
type sandboxRoots struct {
	base    string
	state   string
	config  string
	cache   string
	runtime string
}

// runSandbox prepares the throwaway roots, writes the listener-free config, and
// runs the daemon in the foreground until the operator stops it.
func runSandbox(ctx context.Context, f *cli.Factory, keep bool) error {
	self, err := os.Executable()
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.sandbox.executable_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
		return fmt.Errorf("resolve the running clyde binary: %w", err)
	}

	roots, err := newSandboxRoots()
	if err != nil {
		return err
	}
	if !keep {
		defer func() { _ = os.RemoveAll(roots.base) }()
	}

	configPath := filepath.Join(roots.config, "clyde", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.sandbox.config_dir_failed", "concern", "cmd.dispatch", "component", "cli", "path", configPath, "err", err)
		return fmt.Errorf("create the sandbox config directory %s: %w", filepath.Dir(configPath), err)
	}
	sandboxConfig := fmt.Sprintf(sandboxConfigTemplate, sandboxCollectionID)
	if err := os.WriteFile(configPath, []byte(sandboxConfig), 0o600); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.sandbox.config_write_failed", "concern", "cmd.dispatch", "component", "cli", "path", configPath, "err", err)
		return fmt.Errorf("write the sandbox config %s: %w", configPath, err)
	}

	writeSandboxBanner(f, roots, configPath)

	daemonCmd := exec.CommandContext(ctx, self, "daemon", "run")
	daemonCmd.Env = append(os.Environ(), roots.env()...)
	daemonCmd.Stdout = f.IOStreams.Out
	daemonCmd.Stderr = f.IOStreams.Err
	daemonCmd.Stdin = nil

	// Ctrl-C reaches this process and the daemon alike because they share a
	// process group. Registering the signals stops this process from dying on
	// the first one, so it stays alive to wait for the daemon's own shutdown and
	// to run the deferred cleanup afterwards. Nothing reads the channel: the
	// registration is the whole mechanism, and a full buffer simply drops the
	// repeat signals, which are already reaching the daemon directly.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	if err := daemonCmd.Run(); err != nil {
		// A daemon stopped by Ctrl-C exits non-zero, which is the ordinary way
		// this command ends rather than a failure worth reporting as one.
		slog.InfoContext(ctx, "cli.daemon.sandbox.stopped", "concern", "cmd.dispatch", "component", "cli", "err", err)
	}
	return nil
}

// newSandboxRoots creates the throwaway directory set. The runtime root sits
// directly under /tmp so the daemon's Unix socket path stays within macOS's
// sun_path limit.
func newSandboxRoots() (sandboxRoots, error) {
	var empty sandboxRoots
	base, err := os.MkdirTemp("/tmp", sandboxRootPrefix)
	if err != nil {
		slog.Error("cli.daemon.sandbox.root_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
		return empty, fmt.Errorf("create the sandbox root directory: %w", err)
	}
	roots := sandboxRoots{
		base:    base,
		state:   filepath.Join(base, "state"),
		config:  filepath.Join(base, "config"),
		cache:   filepath.Join(base, "cache"),
		runtime: filepath.Join(base, "run"),
	}
	for _, dir := range []string{roots.state, roots.config, roots.cache, roots.runtime} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			slog.Error("cli.daemon.sandbox.dir_failed", "concern", "cmd.dispatch", "component", "cli", "path", dir, "err", err)
			return empty, fmt.Errorf("create the sandbox directory %s: %w", dir, err)
		}
	}
	return roots, nil
}

// env returns the redirections that move every clyde-owned path into the
// sandbox. A CLI invocation carrying the same values talks to the sandbox
// daemon rather than the deployed one, because both resolve the socket from
// these directories.
func (r sandboxRoots) env() []string {
	return []string{
		"XDG_STATE_HOME=" + r.state,
		"XDG_CONFIG_HOME=" + r.config,
		"XDG_CACHE_HOME=" + r.cache,
		"XDG_RUNTIME_DIR=" + r.runtime,
	}
}

// exportLine renders the redirections as one shell prefix the operator can paste
// in front of a clyde command.
func (r sandboxRoots) exportLine() string {
	return strings.Join(r.env(), " ")
}

// writeSandboxBanner prints what the sandbox is and how to drive it, so the
// operator does not have to derive the environment from the source.
func writeSandboxBanner(f *cli.Factory, roots sandboxRoots, configPath string) {
	out := f.IOStreams.Out
	_, _ = fmt.Fprintln(out, "sandbox daemon")
	_, _ = fmt.Fprintf(out, "  root:   %s\n", roots.base)
	_, _ = fmt.Fprintf(out, "  config: %s\n", configPath)
	_, _ = fmt.Fprintln(out, "  reads:  the real provider conversation stores, with an empty cache")
	_, _ = fmt.Fprintf(out, "  embeds: into the live search engine, collection %q\n", sandboxCollectionID)
	_, _ = fmt.Fprintln(out, "  binds:  nothing, every listener is disabled")
	_, _ = fmt.Fprintln(out, "")
	_, _ = fmt.Fprintln(out, "drive it from another terminal with this prefix:")
	_, _ = fmt.Fprintf(out, "  %s clyde conversation list\n", roots.exportLine())
	_, _ = fmt.Fprintln(out, "")
	_, _ = fmt.Fprintln(out, "Ctrl-C stops it.")
	_, _ = fmt.Fprintln(out, "")
}
