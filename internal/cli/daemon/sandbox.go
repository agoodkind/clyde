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
	daemonCmd.Env = append(sandboxParentEnv(), roots.env()...)
	daemonCmd.Stdout = f.IOStreams.Out
	daemonCmd.Stderr = f.IOStreams.Err
	daemonCmd.Stdin = nil

	// Registering the signals stops this process from dying on the first one, so
	// it stays alive to wait for the daemon's shutdown and to clean up afterwards.
	// A terminal delivers Ctrl-C to the whole foreground process group, so the
	// daemon usually receives it too, but a sandbox started from a script or a
	// supervisor may not share that group. Forwarding explicitly means the daemon
	// is asked to stop in either case rather than being left running.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	// Start before watching for signals, so the daemon's process handle exists by
	// the time anything could forward one to it. Run would leave a window where a
	// signal arriving early found no process to pass it to, and the wrapper would
	// report a clean stop for a daemon that never received one.
	if err := daemonCmd.Start(); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.sandbox.daemon_start_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
		return fmt.Errorf("start the sandbox daemon: %w", err)
	}

	stopped := make(chan struct{})
	forwarded := make(chan struct{})
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(ctx, "cli.daemon.sandbox.signal_forward_panic", "concern", "cmd.dispatch", "component", "cli", "err", fmt.Errorf("forwarding a stop signal panicked: %v", recovered))
			}
		}()
		forwardSignalsToDaemon(ctx, signals, daemonCmd, stopped, forwarded)
	}()

	runErr := daemonCmd.Wait()
	close(stopped)
	if runErr == nil {
		return nil
	}

	// Whether a stop signal arrived is what separates the two ways this ends. An
	// operator stopping the sandbox is success; the daemon exiting on its own is a
	// failure they must see, because a sandbox that never started would otherwise
	// look exactly like one they stopped.
	select {
	case <-forwarded:
		slog.InfoContext(ctx, "cli.daemon.sandbox.stopped", "concern", "cmd.dispatch", "component", "cli", "err", runErr)
		return nil
	default:
		slog.ErrorContext(ctx, "cli.daemon.sandbox.daemon_failed", "concern", "cmd.dispatch", "component", "cli", "err", runErr)
		return fmt.Errorf("the sandbox daemon exited on its own, so it never became usable: %w", runErr)
	}
}

// forwardSignalsToDaemon passes a stop signal on to the sandbox daemon and
// closes forwarded to record that one arrived. It returns when the daemon exits.
func forwardSignalsToDaemon(
	ctx context.Context,
	signals <-chan os.Signal,
	daemonCmd *exec.Cmd,
	stopped <-chan struct{},
	forwarded chan<- struct{},
) {
	select {
	case received := <-signals:
		close(forwarded)
		if daemonCmd.Process == nil {
			return
		}
		if err := daemonCmd.Process.Signal(received); err != nil {
			slog.WarnContext(ctx, "cli.daemon.sandbox.signal_forward_failed", "concern", "cmd.dispatch", "component", "cli", "signal", received.String(), "err", err)
		}
	case <-stopped:
	}
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

// clydeOwnedPathVars name the environment variables that point clyde at a file
// or directory it owns. Each one overrides a path the XDG redirections would
// otherwise place inside the sandbox, so an operator who has any of them set
// would get a sandbox writing into their deployed daemon's logs or state. They
// are dropped from the child's environment rather than redirected, so the
// sandbox falls back to its own roots.
//
// Variables naming a provider's own data are deliberately absent from this list.
// Reading the operator's real conversations is the point of the sandbox, so
// CLYDE_CURSOR_DATA_DIRS and its siblings pass through untouched.
var clydeOwnedPathVars = []string{
	"CLYDE_ANTHROPIC_LOG_PATH",
	"CLYDE_CODEX_LOG_PATH",
	"CLYDE_SLOG_PATH",
	"CLYDE_DAEMON_INHERITED_LISTENERS",
	"CLYDE_DAEMON_READY_FD",
	"CLYDE_DAEMON_RELOAD_CHILD",
	"CLYDE_DAEMON_SUPERVISOR_SOCKET",
	"CLYDE_DEBUG_PPROF_ADDR",
}

// sandboxParentEnv copies the parent environment without the clyde-owned path
// overrides, so nothing the operator exported can pull the sandbox back onto the
// deployed daemon's files.
func sandboxParentEnv() []string {
	drop := make(map[string]bool, len(clydeOwnedPathVars))
	for _, name := range clydeOwnedPathVars {
		drop[name] = true
	}
	parent := os.Environ()
	kept := make([]string, 0, len(parent))
	for _, entry := range parent {
		name, _, found := strings.Cut(entry, "=")
		if found && drop[name] {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
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
