package config

import (
	"path/filepath"
)

const (
	// ConfigFile is the global config file name.
	ConfigFile = "config.toml"
)

// GlobalConfigPath returns the path to the global config file.
// Respects $XDG_CONFIG_HOME if set, otherwise uses ~/.config/clyde/config.toml.
func GlobalConfigPath() string {
	return filepath.Join(defaultXDGResolver.configRoot(), ConfigFile)
}

// GlobalCacheDir returns the global cache directory for clyde.
// Respects $XDG_CACHE_HOME if set, otherwise uses ~/.cache/clyde.
func GlobalCacheDir() string {
	return defaultXDGResolver.cacheRoot()
}
