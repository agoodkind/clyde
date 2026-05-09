package config

import (
	"os"
	"path/filepath"

	"goodkind.io/clyde/internal/util"
)

const (
	// ClydeDir is the directory name for clyde within .claude/
	ClydeDir = ".claude/clyde"

	// SessionsDir is the subdirectory for sessions
	SessionsDir = "sessions"

	// ConfigFile is the global config file name.
	ConfigFile = "config.toml"
)

// GetSessionsDir returns the path to the sessions directory within the clyde root.
func GetSessionsDir(clydeRoot string) string {
	return filepath.Join(clydeRoot, SessionsDir)
}

// GetSessionDir returns the path to a specific session storage directory.
// Callers should pass the stable storage key, not the mutable session name.
func GetSessionDir(clydeRoot, sessionStorageKey string) string {
	return filepath.Join(GetSessionsDir(clydeRoot), sessionStorageKey)
}

// GlobalConfigPath returns the path to the global config file.
// Respects $XDG_CONFIG_HOME if set, otherwise uses ~/.config/clyde/config.toml.
func GlobalConfigPath() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, _ := os.UserHomeDir()
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "clyde", ConfigFile)
}

// FindProjectRoot determines the project root directory.
// Walks up from cwd looking for a .claude/ directory (Claude Code's project marker).
// If found, returns its parent. If not found, returns cwd.
func FindProjectRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return ProjectRootFromPath(cwd), nil
}

// ProjectRootFromPath determines the project root starting from the given path.
// Walks up looking for a .claude/ directory. If found, returns its parent.
// If not found, returns the starting path.
// Stops at $HOME to avoid treating ~/.claude/ (Claude Code's global config) as a project marker.
func ProjectRootFromPath(startPath string) string {
	absPath, err := filepath.Abs(startPath)
	if err != nil {
		return startPath
	}

	homeDir, err := util.HomeDir()
	if err != nil {
		homeDir = ""
	}

	currentPath := absPath
	for homeDir == "" || currentPath != homeDir {
		claudePath := filepath.Join(currentPath, ".claude")
		info, err := os.Stat(claudePath)
		if err == nil && info.IsDir() {
			return currentPath
		}

		parentPath := filepath.Dir(currentPath)
		if parentPath == currentPath {
			// Reached filesystem root
			break
		}
		currentPath = parentPath
	}

	return absPath
}

// GlobalCacheDir returns the global cache directory for clyde.
// Respects $XDG_CACHE_HOME if set, otherwise uses ~/.cache/clyde.
func GlobalCacheDir() string {
	cacheHome := os.Getenv("XDG_CACHE_HOME")
	if cacheHome == "" {
		home, _ := os.UserHomeDir()
		cacheHome = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheHome, "clyde")
}

// SearchResultCacheDir returns the directory where search result caches are stored.
func SearchResultCacheDir() string {
	return filepath.Join(GlobalCacheDir(), "search-results")
}

// EnsureSearchResultCacheDir creates the search result cache directory if it does not exist.
func EnsureSearchResultCacheDir() error {
	return os.MkdirAll(SearchResultCacheDir(), 0o755)
}

// GlobalDataDir returns the global data directory for clyde.
// Respects $XDG_DATA_HOME if set, otherwise uses ~/.local/share/clyde.
func GlobalDataDir() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "clyde")
}

// GlobalSessionsDir returns the path to the global sessions directory.
func GlobalSessionsDir() string {
	return filepath.Join(GlobalDataDir(), SessionsDir)
}

// EnsureGlobalSessionsDir creates the global sessions directory if it doesn't exist.
func EnsureGlobalSessionsDir() error {
	return os.MkdirAll(GlobalSessionsDir(), 0o755)
}

// GlobalOutputStyleRoot returns the "clyde root" used when computing output style
// paths that live in ~/.claude/output-styles/. The extra path component ensures
// filepath.Join(root, "..", "output-styles") resolves to ~/.claude/output-styles/.
func GlobalOutputStyleRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "clyde")
}
