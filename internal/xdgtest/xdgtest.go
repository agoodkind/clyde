// Package xdgtest directs a test binary's Clyde state and cache directories to a temporary root, preventing tests from reading or writing the operator's live state or cache.
package xdgtest

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

const rootPattern = "clyde-test-xdg-"

// isolatedDirs maps each environment variable to its subdirectory under the temporary root.
var isolatedDirs = map[string]string{
	"XDG_STATE_HOME": "state",
	"XDG_CACHE_HOME": "cache",
}

// Main creates a temporary root, sets XDG_STATE_HOME and XDG_CACHE_HOME under it, runs the tests, removes the root, and returns the exit code for [os.Exit]. A test can override either variable with t.Setenv.
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
