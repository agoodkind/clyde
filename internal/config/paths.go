package config

import (
	"path/filepath"
	"strings"
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

func resolveExportAPIKeyFiles(cfg *ExportConfig, configDir string) {
	if cfg == nil {
		return
	}
	cfg.AnthropicAPIKeyFile = resolveConfiguredFilePath(configDir, cfg.AnthropicAPIKeyFile)
	cfg.OpenAIAPIKeyFile = resolveConfiguredFilePath(configDir, cfg.OpenAIAPIKeyFile)
}

func resolveConfiguredFilePath(configDir string, configuredPath string) string {
	trimmedPath := strings.TrimSpace(configuredPath)
	if trimmedPath == "" {
		return ""
	}
	resolvedPath := cleanExpandedPath(trimmedPath)
	if filepath.IsAbs(resolvedPath) {
		return resolvedPath
	}
	return filepath.Join(configDir, resolvedPath)
}
