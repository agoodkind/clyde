// Package xdgtest sets XDG_STATE_HOME and XDG_CACHE_HOME for the test process to a temporary root. All packages in the process that use those variables read and write under that root instead of the operator's state or cache.
package xdgtest

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

const (
	rootPattern     = "clyde-test-xdg-"
	goCacheEnv      = "GOCACHE"
	goBuildCacheDir = "go-build"
)

// isolatedDirs maps each environment variable to its subdirectory under the temporary root.
var isolatedDirs = map[string]string{
	"XDG_STATE_HOME": "state",
	"XDG_CACHE_HOME": "cache",
}

// Main creates a temporary root, preserves the Go build cache, sets XDG_STATE_HOME and XDG_CACHE_HOME under the root, runs tests, removes the root, and returns the code for [os.Exit]. Tests can override either variable with t.Setenv.
func Main(m *testing.M) int {
	root, err := os.MkdirTemp("", rootPattern)
	if err != nil {
		slog.Error("xdgtest.root_create_failed", "concern", "test.isolation", "component", "xdgtest", "err", err)
		_, _ = fmt.Fprintln(os.Stderr, fmt.Errorf("create temporary root: %w", err))
		return 1
	}
	defer func() {
		if removeErr := os.RemoveAll(root); removeErr != nil {
			slog.Warn("xdgtest.root_remove_failed", "concern", "test.isolation", "component", "xdgtest", "path", root, "err", removeErr)
		}
	}()
	if err := keepGoBuildCache(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for name, directory := range isolatedDirs {
		if err := os.Setenv(name, filepath.Join(root, directory)); err != nil {
			slog.Error("xdgtest.env_set_failed", "concern", "test.isolation", "component", "xdgtest", "name", name, "err", err)
			_, _ = fmt.Fprintln(os.Stderr, fmt.Errorf("set %s: %w", name, err))
			return 1
		}
	}
	slog.Debug("xdgtest.isolated", "concern", "test.isolation", "component", "xdgtest", "root", root)
	return m.Run()
}

// keepGoBuildCache sets GOCACHE to the default build cache when unset. On Linux, the default is under XDG_CACHE_HOME; after Main changes it, tests that run go build would otherwise start with an empty cache.
func keepGoBuildCache() error {
	if os.Getenv(goCacheEnv) != "" {
		return nil
	}
	userCache, err := os.UserCacheDir()
	if err != nil {
		slog.Error("xdgtest.user_cache_dir_failed", "concern", "test.isolation", "component", "xdgtest", "err", err)
		return fmt.Errorf("find user cache directory: %w", err)
	}
	if err := os.Setenv(goCacheEnv, filepath.Join(userCache, goBuildCacheDir)); err != nil {
		slog.Error("xdgtest.env_set_failed", "concern", "test.isolation", "component", "xdgtest", "name", goCacheEnv, "err", err)
		return fmt.Errorf("set %s: %w", goCacheEnv, err)
	}
	return nil
}
