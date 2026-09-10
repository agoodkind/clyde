package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemonsupervisor"
	"goodkind.io/clyde/internal/deploy"
	"goodkind.io/clyde/internal/homedir"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
	zedstore "goodkind.io/clyde/internal/providers/zed/store"
	"goodkind.io/clyde/internal/slogger"
)

type resetTarget struct {
	Path           string
	Root           string
	RemoveTree     bool
	AllowProtected bool
}

// HardResetScope names one Clyde-owned local data group.
type HardResetScope string

const (
	// HardResetScopeAll resets every default Clyde data group while preserving configuration.
	HardResetScopeAll HardResetScope = ""
	// HardResetScopeDB resets the Clyde database files.
	HardResetScopeDB HardResetScope = "db"
	// HardResetScopeState resets the Clyde runtime and state files.
	HardResetScopeState HardResetScope = "state"
	// HardResetScopeCache resets the derived Clyde conversation cache.
	HardResetScopeCache HardResetScope = "cache"
	// HardResetScopeConfig removes the Clyde configuration directory.
	HardResetScopeConfig HardResetScope = "config"
	// HardResetScopeHooks removes the Clyde hook state directory.
	HardResetScopeHooks HardResetScope = "hooks"
)

// HardResetOptions selects the Clyde-owned data removed by HardResetWithOptions.
type HardResetOptions struct{ Scope HardResetScope }

func daemonReloadLockPath() string {
	return filepath.Join(config.RuntimeDir(), "daemon.reload.lock")
}

func resetFileTarget(path, root string) resetTarget {
	return resetTarget{Path: path, Root: root, RemoveTree: false, AllowProtected: false}
}

func resetLogTarget(path, root string, removeTree bool) resetTarget {
	return resetTarget{Path: path, Root: root, RemoveTree: removeTree, AllowProtected: true}
}

func readRotatedLogTargets(state string) ([]resetTarget, error) {
	entries, err := os.ReadDir(state)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		slog.Warn("daemon.hard_reset.state_logs_read_failed", "concern", "process.daemon.lifecycle", "path", state, "err", err)
		return nil, fmt.Errorf("read Clyde state logs: %w", err)
	}
	targets := make([]resetTarget, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		isLog := strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.gz") || strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".log.gz")
		isClydeLog := strings.HasPrefix(name, "clyde-") || strings.HasPrefix(name, "codex") || strings.HasPrefix(name, "anthropic") || strings.HasPrefix(name, "audit") || strings.HasPrefix(name, "daemon")
		if isLog && isClydeLog {
			targets = append(targets, resetLogTarget(filepath.Join(state, entry.Name()), state, false))
		}
	}
	return targets, nil
}

// HardReset deletes only Clyde's local databases and derived index state, then
// reinstalls this executable through the native user service installer.
func HardReset(ctx context.Context, output io.Writer) (err error) {
	return HardResetWithOptions(ctx, output, HardResetOptions{Scope: HardResetScopeAll})
}

// HardResetWithOptions stops Clyde, removes the selected Clyde-owned data, and
// reinstalls the native service. The default scope preserves configuration.
func HardResetWithOptions(ctx context.Context, output io.Writer, options HardResetOptions) (err error) {
	slog.InfoContext(ctx, "daemon.hard_reset.started", "concern", "process.daemon.lifecycle")
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.hard_reset.failed", "concern", "process.daemon.lifecycle")
		}
	}()
	var cfg *config.Config
	if options.Scope != HardResetScopeConfig {
		cfg, err = config.LoadGlobalOrDefault()
		if err != nil {
			return fmt.Errorf("validate preserved config: %w", err)
		}
	}
	targets, err := hardResetTargetsForScope(ctx, cfg, options.Scope)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve current executable symlinks: %w", err)
	}
	lookup := func(key string) (string, bool) {
		if key == "INSTALL_BIN" {
			return executable, true
		}
		return os.LookupEnv(key)
	}
	processes, err := hardResetProcesses(ctx, executable)
	if err != nil {
		return err
	}
	if err := deploy.RemoveFromEnv(ctx, lookup, output); err != nil {
		return fmt.Errorf("unregister Clyde before reset: %w", err)
	}
	if err := stopResetProcesses(ctx, processes); err != nil {
		return err
	}
	// The supervisor is now gone and cannot spawn replacements. Catch workers
	// it created during teardown, which have reparented to init by this point.
	remaining, err := hardResetProcesses(ctx, executable)
	if err != nil {
		return err
	}
	if err := stopResetProcesses(ctx, remaining); err != nil {
		return err
	}
	for _, target := range targets {
		// Recheck after shutdown because the old generation may have replaced a
		// checkpoint while draining. Never follow a newly introduced symlink.
		if err := validateResetTarget(ctx, target, cfg); err != nil {
			return err
		}
		if target.RemoveTree {
			err = os.RemoveAll(target.Path)
		} else {
			err = os.Remove(target.Path)
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("remove reset target %s: %w", target.Path, err)
		}
		_, _ = fmt.Fprintln(output, "Removed Clyde data:", target.Path)
	}
	if err := deploy.RunFromEnv(ctx, lookup, false, output, output, deploy.Fingerprints{
		Compiled: CompiledSupervisorFingerprint, Running: RunningSupervisorFingerprint,
	}); err != nil {
		return fmt.Errorf("clyde data reset; native installation failed: %w", err)
	}
	_, _ = fmt.Fprintln(output, "Clyde data reset; native service installation and status check succeeded.")
	return nil
}

func hardResetTargetsForScope(ctx context.Context, cfg *config.Config, scope HardResetScope) (_ []resetTarget, err error) {
	defer func() {
		if err != nil {
			slog.Warn("daemon.hard_reset.inventory_rejected", "concern", "process.daemon.lifecycle")
		}
	}()
	state, cache, runtime := config.DefaultStateDir(), config.GlobalCacheDir(), config.RuntimeDir()
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve Clyde fallback log: %w", err)
	}
	fallbackLogRoot := filepath.Join(home, "Library", "Logs")
	cacheTargets := []resetTarget{resetFileTarget(conversation.CachePath(), cache)}
	stateTargets := []resetTarget{
		resetLogTarget(filepath.Join(fallbackLogRoot, "clyde-daemon.log"), fallbackLogRoot, false),
		resetLogTarget(filepath.Join(state, "logs"), state, true),
		resetLogTarget(filepath.Join(state, "mitm-launcher"), state, true),
		resetLogTarget(filepath.Join(state, "clyde-daemon.jsonl"), state, false),
		resetLogTarget(filepath.Join(state, "clyde-cli.jsonl"), state, false),
		resetLogTarget(filepath.Join(state, "codex.jsonl"), state, false),
		resetLogTarget(filepath.Join(state, "anthropic.jsonl"), state, false),
		resetLogTarget(filepath.Join(state, "audit.jsonl"), state, false),
		resetLogTarget(filepath.Join(state, "daemon.log"), state, false),
		resetFileTarget(metricsRollupPath(), state),
		resetFileTarget(metricsRollupPath()+".lock", state),
		resetFileTarget(metricsRollupPath()+".tmp", state),
		resetFileTarget(metricsRollupCheckpointPath(), state),
		resetFileTarget(metricsRollupCheckpointPath()+".tmp", state),
		resetFileTarget(config.DaemonSocketPath(), runtime),
		resetFileTarget(daemonsupervisor.SocketPath(runtime), runtime),
		resetFileTarget(daemonReloadLockPath(), runtime),
	}
	rotatedLogs, err := readRotatedLogTargets(state)
	if err != nil {
		return nil, err
	}
	stateTargets = append(stateTargets, rotatedLogs...)
	if scope == HardResetScopeAll || scope == HardResetScopeState {
		socket, err := config.DaemonSocketPathFromGRPCAddress(cfg.Daemon.GRPCAddress)
		if err != nil {
			return nil, fmt.Errorf("resolve daemon reset socket: %w", err)
		}
		stateTargets = append(stateTargets, resetFileTarget(socket, runtime))
	}
	targets := make([]resetTarget, 0)
	switch scope {
	case HardResetScopeAll:
		targets = append(targets, cacheTargets...)
		targets = append(targets, stateTargets...)
	case HardResetScopeCache:
		targets = append(targets, cacheTargets...)
	case HardResetScopeState:
		targets = append(targets, stateTargets...)
	case HardResetScopeHooks:
		targets = append(targets, resetTarget{Path: filepath.Join(state, "hooks"), Root: state, RemoveTree: true, AllowProtected: false})
	case HardResetScopeConfig:
		configDir := filepath.Dir(config.GlobalConfigPath())
		targets = append(targets, resetTarget{Path: configDir, Root: filepath.Dir(configDir), RemoveTree: true, AllowProtected: true})
	case HardResetScopeDB:
	default:
		return nil, fmt.Errorf("unsupported hard-reset target %q", scope)
	}
	if scope != HardResetScopeAll && scope != HardResetScopeDB {
		for _, target := range targets {
			if err := validateResetTarget(ctx, target, cfg); err != nil {
				return nil, err
			}
		}
		return targets, nil
	}
	capturePath := cfg.MITM.CaptureStore.DBPath
	root := ""
	for _, candidate := range []string{state, cache, runtime} {
		if withinResetRoot(capturePath, candidate) {
			root = candidate
			break
		}
	}
	if root == "" {
		return nil, fmt.Errorf("capture reset target %s is outside Clyde ownership", capturePath)
	}
	if scope == HardResetScopeAll || scope == HardResetScopeDB {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			targets = append(targets, resetFileTarget(capturePath+suffix, root))
		}
	}
	for _, target := range targets {
		if err := validateResetTarget(ctx, target, cfg); err != nil {
			return nil, err
		}
	}
	return targets, nil
}

func withinResetRoot(path, root string) bool {
	if !filepath.IsAbs(path) || !filepath.IsAbs(root) {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validateResetTarget(ctx context.Context, target resetTarget, cfg *config.Config) (err error) {
	defer func() {
		if err != nil {
			slog.Warn("daemon.hard_reset.target_rejected", "concern", "process.daemon.lifecycle")
		}
	}()
	if !withinResetRoot(target.Path, target.Root) {
		return fmt.Errorf("reset target %s is outside Clyde ownership", target.Path)
	}
	for current := filepath.Clean(target.Path); withinResetRoot(current, target.Root); current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect reset target %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || (current == filepath.Clean(target.Path) && info.IsDir() && !target.RemoveTree) {
			return fmt.Errorf("refuse symlink or directory reset target %s", current)
		}
	}
	resolvedTarget, err := resolvedResetPath(target.Path)
	if err != nil {
		return err
	}
	if target.AllowProtected {
		return nil
	}
	for _, path := range resetProtectedFiles(cfg) {
		if path == "" {
			continue
		}
		resolved, err := resolvedResetPath(homedir.Expand(path))
		if err != nil {
			return err
		}
		if resolved == resolvedTarget {
			return fmt.Errorf("reset target %s overlaps protected configuration data", target.Path)
		}
	}
	roots, err := resetProtectedDirectories(ctx, cfg)
	if err != nil {
		return err
	}
	for _, root := range roots {
		resolved, err := resolvedResetPath(root)
		if err != nil {
			return err
		}
		if withinResetRoot(resolvedTarget, resolved) || resolvedTarget == resolved {
			return fmt.Errorf("reset target %s overlaps protected directory %s", target.Path, root)
		}
	}
	return nil
}

func resetProtectedFiles(cfg *config.Config) []string {
	protected := []string{
		config.GlobalConfigPath(), cfg.MITM.CA.CertPath, cfg.MITM.CA.KeyPath,
		cfg.Export.AnthropicAPIKeyFile, cfg.Export.OpenAIAPIKeyFile,
		cfg.Adapter.Codex.AuthFile, cfg.Logging.Paths.CLI, cfg.Logging.Paths.Daemon,
		slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleCLI),
		slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleDaemon),
		os.Getenv("LOG_PATH"), adaptercodex.LogPath(), anthropic.LogPath(),
	}
	for _, model := range cfg.Adapter.Models {
		path := homedir.Expand(model.InstructionsFile)
		if path != "" && !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(config.GlobalConfigPath()), path)
		}
		protected = append(protected, path)
	}
	for _, spec := range config.LoggingSinkSpecs() {
		if override, ok := cfg.Logging.Sinks.Override(spec.Name); ok {
			protected = append(protected, override.Path)
		}
	}
	return protected
}

func resolvedResetPath(path string) (_ string, err error) {
	defer func() {
		if err != nil {
			slog.Warn("daemon.hard_reset.path_resolution_failed", "concern", "process.daemon.lifecycle")
		}
	}()
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute reset path: %w", err)
	}
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			for _, part := range slices.Backward(tail) {
				resolved = filepath.Join(resolved, part)
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolve reset boundary %s: %w", path, err)
		}
		tail = append(tail, filepath.Base(path))
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("resolve filesystem reset root: %w", err)
		}
		path = parent
	}
}

func resetProtectedDirectories(ctx context.Context, cfg *config.Config) (_ []string, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.hard_reset.protected_roots_failed", "concern", "process.daemon.lifecycle")
		}
	}()
	codex, err := codexstore.ResolveStorePathsFromEnv(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve protected Codex roots: %w", err)
	}
	cursor, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve protected Cursor roots: %w", err)
	}
	zed, err := zedstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve protected Zed roots: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve protected provider home: %w", err)
	}
	roots := []string{
		codex.CodexHome, codex.SQLiteHome,
		oauthcredentials.ResolveDirectory(home, ""),
		oauthcredentials.ResolveDirectory(home, os.Getenv("CLAUDE_CONFIG_DIR")),
		slogger.DefaultConcernRoot(cfg.Logging, slogger.ProcessRoleCLI),
		slogger.DefaultConcernRoot(cfg.Logging, slogger.ProcessRoleDaemon),
		filepath.Join(config.DefaultStateDir(), "exports"),
	}
	for _, root := range cursor {
		roots = append(roots, root.RootDir)
	}
	for _, root := range zed {
		roots = append(roots, root.RootDir)
	}
	return roots, nil
}
