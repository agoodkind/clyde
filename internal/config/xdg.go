package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	uid := r.uid()
	if base, ok := xdgBaseFromEnv("TMPDIR"); ok {
		return filepath.Join(base, fmt.Sprintf("%s-%d", r.appName, uid))
	}
	return filepath.Join(cleanExpandedPath(os.TempDir()), fmt.Sprintf("%s-%d", r.appName, uid))
}

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
	return filepath.Clean(expandLeadingHome(path))
}

func expandLeadingHome(path string) string {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
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

// RuntimeDir returns a user-scoped runtime directory for the daemon socket.
// Uses XDG_RUNTIME_DIR if set, then TMPDIR, then a UID-scoped fallback.
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
		log.Warn("config.runtime_dir.create_failed", "concern", "config", "component", "config",
			"subcomponent", "runtime_dir",
			"path", dir,
			"err", err,
		)
		return fmt.Errorf("failed to create runtime dir %s: %w", dir, err)
	}
	return nil
}
