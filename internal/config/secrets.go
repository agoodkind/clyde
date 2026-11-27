package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/util"
)

var (
	errSecretSourceConflict = errors.New("credential sources are mutually exclusive")
	errEmptySecretReference = errors.New("credential reference is empty")
	errConfigAbsent         = errors.New("configuration file does not exist")
)

// ResolveRequireToken reads the configured inbound token.
//
// A configured reference must contain a value and exclude other sources.
//
// Example:
//
//	cfg := AdapterConfig{RequireTokenEnv: new("ADAPTER_TOKEN")}
//	token, err := cfg.ResolveRequireToken()
func (cfg AdapterConfig) ResolveRequireToken() (string, error) {
	if cfg.RequireTokenEnv == nil && cfg.RequireTokenFile == nil {
		return cfg.RequireToken, nil
	}

	if cfg.RequireToken != "" || (cfg.RequireTokenEnv != nil && cfg.RequireTokenFile != nil) {
		slog.Warn(
			"config.adapter.token_source_conflict",
			"component",
			"config",
			"err",
			errSecretSourceConflict,
		)

		return "", fmt.Errorf(
			"adapter.require_token, require_token_env, require_token_file: %w",
			errSecretSourceConflict,
		)
	}

	if cfg.RequireTokenEnv != nil {
		return readAdapterSecret("adapter.require_token_env", *cfg.RequireTokenEnv, "")
	}

	return readAdapterSecret("adapter.require_token_file", "", *cfg.RequireTokenFile)
}

// ResolvePassthroughAPIKey reads a passthrough credential.
//
// A file excludes other sources. A literal takes precedence over an
// environment key.
//
// Example:
//
//	key, err := ResolvePassthroughAPIKey(
//		"adapter.openai_compat_passthrough", "", "UPSTREAM_KEY", nil,
//	)
func ResolvePassthroughAPIKey(setting, literal, environment string, path *string) (string, error) {
	return resolveCredentialSources(setting+".api_key", literal, environment, path, false)
}

func resolveCredentialSources(
	setting string,
	literal string,
	environment string,
	path *string,
	requireOne bool,
) (string, error) {
	if (path != nil && (literal != "" || environment != "")) ||
		(requireOne && literal != "" && environment != "") {
		slog.Warn(
			"config.adapter.key_source_conflict",
			"component",
			"config",
			"setting",
			setting,
			"err",
			errSecretSourceConflict,
		)

		return "", fmt.Errorf(
			"%s, %s_env, %s_file: %w",
			setting,
			setting,
			setting,
			errSecretSourceConflict,
		)
	}

	if path != nil {
		return readAdapterSecret(setting+"_file", "", *path)
	}

	if literal != "" {
		return literal, nil
	}

	if environment != "" {
		return readAdapterSecret(setting+"_env", environment, "")
	}

	if requireOne {
		return "", fmt.Errorf(
			"%s, %s_env, %s_file: %w",
			setting,
			setting,
			setting,
			errEmptySecretReference,
		)
	}

	return "", nil
}

func readAdapterSecret(setting, environment, path string) (string, error) {
	if strings.TrimSpace(environment) == "" && strings.TrimSpace(path) == "" {
		slog.Warn(
			"config.adapter.secret_reference_empty",
			"component",
			"config",
			"setting",
			setting,
			"err",
			errEmptySecretReference,
		)

		return "", fmt.Errorf("%s: %w", setting, errEmptySecretReference)
	}

	value, err := util.ReadSecretReference(setting, environment, path)
	if err != nil {
		return "", fmt.Errorf("resolve adapter credential: %w", err)
	}

	return value, nil
}

// ValidateSecrets checks adapter credential references.
//
// A failed check prevents configuration replacement.
//
// Example:
//
//	if err := cfg.ValidateSecrets(); err != nil {
//		return err
//	}
func (cfg AdapterConfig) ValidateSecrets() error {
	if _, err := cfg.ResolveRequireToken(); err != nil {
		return err
	}

	if cfg.Antigravity.Enabled {
		if _, _, err := cfg.Antigravity.ResolveOAuthClient(); err != nil {
			return err
		}
	}

	passthrough := cfg.OpenAICompatPassthrough
	if _, err := ResolvePassthroughAPIKey(
		"adapter.openai_compat_passthrough",
		passthrough.APIKey,
		passthrough.APIKeyEnv,
		passthrough.APIKeyFile,
	); err != nil {
		return err
	}

	for name, override := range cfg.PassthroughOverrides {
		if _, err := ResolvePassthroughAPIKey(
			"adapter.passthrough_overrides."+name,
			override.APIKey,
			override.APIKeyEnv,
			override.APIKeyFile,
		); err != nil {
			return err
		}
	}

	return nil
}

func resolveAdapterSecretPaths(cfg *AdapterConfig, directory string) {
	cfg.RequireTokenFile = resolveSecretPath(directory, cfg.RequireTokenFile)
	cfg.Antigravity.ClientSecretFile = resolveSecretPath(
		directory,
		cfg.Antigravity.ClientSecretFile,
	)
	cfg.OpenAICompatPassthrough.APIKeyFile = resolveSecretPath(
		directory,
		cfg.OpenAICompatPassthrough.APIKeyFile,
	)

	for name, override := range cfg.PassthroughOverrides {
		override.APIKeyFile = resolveSecretPath(directory, override.APIKeyFile)
		cfg.PassthroughOverrides[name] = override
	}
}

func resolveSecretPath(directory string, path *string) *string {
	if path == nil || strings.TrimSpace(*path) == "" {
		return path
	}

	resolved := resolveConfiguredFilePath(directory, *path)

	return &resolved
}
