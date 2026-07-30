// Package sandbox roots a daemon inside one throwaway directory and on
// throwaway ports, so it cannot reach the operator's state, config, socket, or
// listeners.
//
// Every value is a default: a name already set in the environment is left
// alone. The sandbox command and the live suite both resolve through here, so
// what a hand-run daemon isolates and what the suite isolates cannot drift.
package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	stateDirName   = "state"
	configDirName  = "config"
	cacheDirName   = "cache"
	runtimeDirName = "run"

	// RootParent is where a sandbox root is created. It is /tmp because the
	// runtime directory beneath it holds the daemon's Unix socket, whose path
	// must fit the platform's roughly 104 byte limit, and the platform temp
	// directory overflows that.
	RootParent = "/tmp"

	// RootPattern prefixes each sandbox root directory.
	RootPattern = "clyde-sandbox-"

	rootDirMode = 0o700
)

// Var is one environment default the sandbox supplies.
type Var struct {
	Name  string
	Value string
}

// Roots are the directories one sandbox daemon reads and writes.
type Roots struct {
	Base    string
	State   string
	Config  string
	Cache   string
	Runtime string
}

// NewRoots creates one sandbox's directory set under /tmp. The caller removes
// Base to clean up.
func NewRoots() (Roots, error) {
	base, err := os.MkdirTemp(RootParent, RootPattern)
	if err != nil {
		slog.Error("sandbox.roots.create_failed", "concern", "process.daemon.lifecycle", "component", "sandbox", "parent", RootParent, "err", err)
		return Roots{}, fmt.Errorf("create the sandbox root directory: %w", err)
	}
	roots := Roots{
		Base:    base,
		State:   filepath.Join(base, stateDirName),
		Config:  filepath.Join(base, configDirName),
		Cache:   filepath.Join(base, cacheDirName),
		Runtime: filepath.Join(base, runtimeDirName),
	}
	for _, dir := range []string{roots.State, roots.Config, roots.Cache, roots.Runtime} {
		if mkErr := os.MkdirAll(dir, rootDirMode); mkErr != nil {
			slog.Error("sandbox.roots.create_failed", "concern", "process.daemon.lifecycle", "component", "sandbox", "path", dir, "err", mkErr)
			_ = os.RemoveAll(base)
			return Roots{}, fmt.Errorf("create the sandbox directory %s: %w", dir, mkErr)
		}
	}
	slog.Debug("sandbox.roots.created", "concern", "process.daemon.lifecycle", "component", "sandbox", "base", base)
	return roots, nil
}

// Env returns the redirections that move every clyde-owned path into roots, in
// a fixed order. Returning the table rather than applying it lets a test
// install the same values through t.Setenv, which unwinds when the test ends,
// and lets a caller print them as a shell prefix.
func Env(roots Roots) []Var {
	return []Var{
		{Name: "XDG_STATE_HOME", Value: roots.State},
		{Name: "XDG_CONFIG_HOME", Value: roots.Config},
		{Name: "XDG_CACHE_HOME", Value: roots.Cache},
		{Name: "XDG_RUNTIME_DIR", Value: roots.Runtime},
	}
}

// ExportLine renders the redirections as one shell prefix, so a second terminal
// can drive the sandbox daemon rather than the deployed one.
func ExportLine(roots Roots) string {
	parts := make([]string, 0, len(Env(roots)))
	for _, variable := range Env(roots) {
		parts = append(parts, variable.Name+"="+variable.Value)
	}
	return strings.Join(parts, " ")
}

// inheritedPathVars name the variables that point clyde at a file or directory
// it owns. Each one overrides a path the redirections would otherwise place
// inside the sandbox, so an operator who exported any of them would get a
// sandbox writing into their deployed daemon's logs or state. Clearing them
// leaves the sandbox on its own roots.
//
// Variables naming a provider's own data are deliberately absent. Reading the
// operator's real conversations is the point, so CLYDE_CURSOR_DATA_DIRS and its
// siblings pass through untouched.
var inheritedPathVars = []string{
	"CLYDE_ANTHROPIC_LOG_PATH",
	"CLYDE_CODEX_LOG_PATH",
	"CLYDE_SLOG_PATH",
	"CLYDE_DAEMON_INHERITED_LISTENERS",
	"CLYDE_DAEMON_READY_FD",
	"CLYDE_DAEMON_RELOAD_CHILD",
	"CLYDE_DAEMON_SUPERVISOR_SOCKET",
	"CLYDE_DEBUG_PPROF_ADDR",
}

// Apply points this process at roots and clears any inherited override that
// would pull it back onto the deployed daemon's files. The daemon runs in this
// process, so the redirections have to take effect here rather than in a child
// environment.
func Apply(roots Roots) error {
	for _, name := range inheritedPathVars {
		if err := os.Unsetenv(name); err != nil {
			slog.Error("sandbox.env.clear_failed", "concern", "process.daemon.lifecycle", "component", "sandbox", "name", name, "err", err)
			return fmt.Errorf("clear inherited override %s: %w", name, err)
		}
	}
	for _, variable := range Env(roots) {
		if err := os.Setenv(variable.Name, variable.Value); err != nil {
			slog.Error("sandbox.env.set_failed", "concern", "process.daemon.lifecycle", "component", "sandbox", "name", variable.Name, "err", err)
			return fmt.Errorf("set %s: %w", variable.Name, err)
		}
	}
	slog.Debug("sandbox.env.applied", "concern", "process.daemon.lifecycle", "component", "sandbox", "base", roots.Base)
	return nil
}

// PreflightRoots refuses to proceed when a root sits outside a temp directory,
// so a mistake in how the roots were built fails before anything is written.
func PreflightRoots(roots Roots) error {
	for _, root := range []string{roots.Base, roots.State, roots.Config, roots.Cache, roots.Runtime} {
		if !UnderTempRoot(root) {
			err := fmt.Errorf("sandbox root %q is not a temp directory", root)
			slog.Error("sandbox.preflight.root_rejected", "concern", "process.daemon.lifecycle", "component", "sandbox", "path", root, "err", err)
			return err
		}
	}
	return nil
}

// UnderTempRoot reports whether path lives under a temp root. It matches on a
// path-segment boundary, so a production path that merely contains "tmp" as a
// substring is not mistaken for a throwaway one.
func UnderTempRoot(path string) bool {
	clean := filepath.Clean(path)
	for _, root := range tempRoots() {
		if clean == root || strings.HasPrefix(clean, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// tempRoots returns the directories treated as temp: the OS temp dir plus the
// explicit /tmp and /private roots macOS resolves it through.
func tempRoots() []string {
	roots := []string{"/tmp", "/private/tmp", "/private/var/folders"}
	if osTemp := filepath.Clean(os.TempDir()); osTemp != "" && osTemp != "." {
		roots = append(roots, osTemp)
	}
	return roots
}
