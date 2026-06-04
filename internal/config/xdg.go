package config

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/homedir"
)

const appName = "clyde"

type xdgResolver struct {
	appName string
	uid     func() int
}

var defaultXDGResolver = xdgResolver{
	appName: appName,
	uid:     os.Getuid,
}

func (r xdgResolver) configRoot() string {
	return r.appScopedRoot("XDG_CONFIG_HOME", ".config")
}

func (r xdgResolver) cacheRoot() string {
	return r.appScopedRoot("XDG_CACHE_HOME", ".cache")
}

func (r xdgResolver) stateRoot() string {
	return r.appScopedRoot("XDG_STATE_HOME", filepath.Join(".local", "state"))
}

func (r xdgResolver) runtimeRoot() string {
	if base, ok := xdgBaseFromEnv("XDG_RUNTIME_DIR"); ok {
		return filepath.Join(base, r.appName)
	}
	return filepath.Join(platformRuntimeBase(), fmt.Sprintf("%s-%d", r.appName, r.uid()))
}

// platformRuntimeBase returns the per-user runtime base directory used when
// XDG_RUNTIME_DIR is unset. On macOS it reads the Darwin per-user temp dir from
// the OS, independent of the $TMPDIR environment variable, so the daemon and
// every client agree on the socket path even when a sandbox overrides $TMPDIR.
// Elsewhere it uses [os.TempDir].
func platformRuntimeBase() string {
	if runtime.GOOS == "darwin" {
		if dir := darwinUserTempDir(); dir != "" {
			return cleanExpandedPath(dir)
		}
	}
	return cleanExpandedPath(os.TempDir())
}

// darwinUserTempDir resolves and caches the Darwin per-user temp directory from
// the OS via getconf, independent of the $TMPDIR environment variable. It
// returns the empty string when the lookup fails, so callers fall back to
// [os.TempDir].
var darwinUserTempDir = sync.OnceValue(func() string {
	out, err := exec.Command("getconf", "DARWIN_USER_TEMP_DIR").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
})

func (r xdgResolver) appScopedRoot(envVar string, fallbackRelative string) string {
	if base, ok := xdgBaseFromEnv(envVar); ok {
		return filepath.Join(base, r.appName)
	}
	return filepath.Join(homeRelativeRoot(fallbackRelative), r.appName)
}

func xdgBaseFromEnv(envVar string) (string, bool) {
	value := strings.TrimSpace(os.Getenv(envVar))
	if value == "" {
		return "", false
	}
	return cleanExpandedPath(value), true
}

func homeRelativeRoot(relativePath string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return cleanExpandedPath(filepath.Join(home, relativePath))
}

func cleanExpandedPath(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Clean(homedir.Expand(path))
}

// DefaultStateDir returns the XDG-derived state directory for clyde.
//
// Resolution:
//
//	$XDG_STATE_HOME/clyde    (if $XDG_STATE_HOME is set)
//	~/.local/state/clyde      (XDG spec default)
func DefaultStateDir() string {
	return defaultXDGResolver.stateRoot()
}

// RuntimeDir returns a user-scoped runtime directory for the daemon socket. It
// uses XDG_RUNTIME_DIR when set, otherwise the OS per-user runtime directory
// resolved independently of $TMPDIR (the Darwin user temp dir on macOS), so
// every clyde process agrees on the path regardless of the environment.
func RuntimeDir() string {
	return defaultXDGResolver.runtimeRoot()
}

// DaemonSocketPath returns the Unix socket path for the clyde daemon.
func DaemonSocketPath() string {
	return filepath.Join(RuntimeDir(), "daemon.sock")
}

// EnsureRuntimeDir creates the clyde runtime directory with correct permissions.
// XDG spec requires 0700 for XDG_RUNTIME_DIR contents.
func EnsureRuntimeDir() error {
	dir := RuntimeDir()
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		log := slog.Default()
		log.Warn(
			"config.runtime_dir.create_failed", "concern", "config", "component", "config",
			"subcomponent", "runtime_dir",
			"path", dir,
			"err", err,
		)
		return fmt.Errorf("failed to create runtime dir %s: %w", dir, err)
	}
	return nil
}
