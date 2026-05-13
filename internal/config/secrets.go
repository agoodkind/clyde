package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

func resolveConfigSecrets(cfg *Config, configPath string) error {
	if cfg == nil {
		return nil
	}
	configDir := filepath.Dir(configPath)
	if err := resolveConfigSecret(configDir, "defaults.anthropic_api_key", cfg.Defaults.AnthropicAPIKey, &cfg.Defaults.resolvedAnthropicAPIKey); err != nil {
		return err
	}
	if err := resolveConfigSecret(configDir, "web_app.require_token", cfg.WebApp.RequireToken, &cfg.WebApp.resolvedRequireToken); err != nil {
		return err
	}
	if err := resolveConfigSecret(configDir, "adapter.require_token", cfg.Adapter.RequireToken, &cfg.Adapter.resolvedRequireToken); err != nil {
		return err
	}
	if err := resolveConfigSecret(configDir, "adapter.openai_compat_passthrough.api_key", cfg.Adapter.OpenAICompatPassthrough.APIKey, &cfg.Adapter.OpenAICompatPassthrough.resolvedAPIKey); err != nil {
		return err
	}
	if err := resolveConfigSecret(configDir, "search.local.token", cfg.Search.Local.Token, &cfg.Search.Local.resolvedToken); err != nil {
		return err
	}
	if err := resolveConfigSecret(configDir, "search.local.embedding_token", cfg.Search.Local.EmbeddingToken, &cfg.Search.Local.resolvedEmbeddingToken); err != nil {
		return err
	}
	for name, override := range cfg.Adapter.PassthroughOverrides {
		fieldPath := "adapter.passthrough_overrides." + name + ".api_key"
		if err := resolveConfigSecret(configDir, fieldPath, override.APIKey, &override.resolvedAPIKey); err != nil {
			return err
		}
		cfg.Adapter.PassthroughOverrides[name] = override
	}
	return nil
}

func resolveConfigSecret(configDir string, fieldPath string, value string, resolved *string) error {
	secret, err := ResolveSecretValue(configDir, fieldPath, value)
	if err != nil {
		return err
	}
	*resolved = secret
	return nil
}

// ResolveSecretValue returns the secret value for a config field. Inline values
// pass through unchanged. Definite path values and existing relative path
// values are read from disk relative to the config file directory.
func ResolveSecretValue(configDir string, fieldPath string, value string) (string, error) {
	log := slog.Default().With("concern", "process.daemon.config")
	trimmedValue := strings.TrimSpace(value)
	if trimmedValue == "" {
		return "", nil
	}
	secretPath, isPath, err := secretPathCandidate(configDir, trimmedValue)
	if err != nil {
		return "", err
	}
	if !isPath {
		return value, nil
	}
	info, err := os.Stat(secretPath)
	if err != nil {
		log.Warn("config.secret.resolve_failed",
			"component", "config",
			"subcomponent", "secret",
			"field", fieldPath,
			"path", secretPath,
			"err", err,
		)
		return "", fmt.Errorf("%s: read secret file %q: %w", fieldPath, secretPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s: secret file %q is a directory", fieldPath, secretPath)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s: secret file %q is not a regular file", fieldPath, secretPath)
	}
	contents, err := os.ReadFile(secretPath)
	if err != nil {
		log.Warn("config.secret.resolve_failed",
			"component", "config",
			"subcomponent", "secret",
			"field", fieldPath,
			"path", secretPath,
			"err", err,
		)
		return "", fmt.Errorf("%s: read secret file %q: %w", fieldPath, secretPath, err)
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" {
		return "", fmt.Errorf("%s: secret file %q is empty", fieldPath, secretPath)
	}
	return secret, nil
}

func secretPathCandidate(configDir string, value string) (string, bool, error) {
	log := slog.Default().With("concern", "process.daemon.config")
	expandedPath := expandHome(value)
	if filepath.IsAbs(expandedPath) {
		return filepath.Clean(expandedPath), true, nil
	}
	if strings.HasPrefix(value, "~/") || value == "~" {
		return filepath.Clean(expandedPath), true, nil
	}
	if strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") {
		return filepath.Clean(filepath.Join(configDir, expandedPath)), true, nil
	}
	if strings.Contains(value, "/") {
		secretPath := filepath.Clean(filepath.Join(configDir, expandedPath))
		_, err := os.Stat(secretPath)
		if err == nil {
			return secretPath, true, nil
		}
		if os.IsNotExist(err) {
			return "", false, nil
		}
		log.Warn("config.secret.resolve_failed",
			"component", "config",
			"subcomponent", "secret",
			"path", secretPath,
			"err", err,
		)
		return "", false, fmt.Errorf("stat secret file %q: %w", secretPath, err)
	}
	return "", false, nil
}
